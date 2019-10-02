/*
scan_util is a program for managing the process of moving accumulated image
files created by a photo/film scanner to a gallery.

Copyright 2018 Jim Lieb <lieb@sea-troll.net>

This software is licensed under the terms of the GNU Public License V3 or later
See the LICENSE file this repository or at https://fsf.org for details.
*/

package main

import (
	"errors"
	"fmt"
	"github.com/pborman/getopt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	Ps = string(os.PathSeparator)
	Fs = "-"
)

// Error codes

var (
	ErrUnknownFile   = errors.New(`Could not identify file using EXIF data.`)
	ErrNotADirectory = errors.New(`%s: is not a directory.`)
)

// Command line options and args
var (
	def_date   = time.Now().Format("Jan-2006")
	def_foto   = "James Lieb"
	scan_types = []string{"TR", "CN", "BW"}

	src_dir = getopt.StringLong("src_dir", 'i',
		".", "Source directory")
	dst_dir = getopt.StringLong("dst_dir", 'o',
		".", "Destination directory")
	orig_date = getopt.StringLong("date", 'D',
		def_date, "Original processing date as 'mon-year' or 'mon day, year'")
	film_type = getopt.EnumLong("film_type", 'T',
		scan_types,
		"Film type: TR = slide, CN = color neg, BW = B/W neg")
	batch = getopt.IntLong("batch", 'b',
		1, "Processing box or batch number")
	file_suffix = getopt.StringLong("suffix", 'S',
		"dng", "File name suffix to match. default = dng")
	max_procs = getopt.IntLong("max-procs", 'M',
		runtime.NumCPU(), "Number of concurrent jobs")
	description = getopt.StringLong("description", 'd',
		"", "Image description")
	comment = getopt.StringLong("comment", 'c',
		"", "User comments")
	photographer = getopt.StringLong("photographer", 'p',
		def_foto, "Name of original photographer/artist")
	create_jpeg = getopt.BoolLong("jpeg", 'J',
		"Generate a jpeg format image file. Off by default")
	show_progress = getopt.BoolLong("show-progress", 'v',
		"Show a series of ...'s, one as each one is done")
	optHelp = getopt.BoolLong("help", 'h', "Help")
)

type exiv2_cmd struct {
	tag      string
	tag_type string
	value    string
}

// Contents of the exiv2 command file

var (
	copyright_fmt = exiv2_cmd{
		"Exif.Image.Copyright",
		"Ascii",
		"Copyright %s, %s. All rights reserved"}
	artist_fmt = exiv2_cmd{
		"Exif.Image.Artist",
		"Ascii", "%s"}
	origdate_fmt = exiv2_cmd{
		"Exif.Photo.DateTimeOriginal",
		"Ascii", "%4d:%02d:%02d 00:00:00"}
	desc_fmt = exiv2_cmd{
		"Exif.Image.ImageDescription",
		"Ascii",
		"%s"}
	docname_fmt = exiv2_cmd{
		"Exif.Image.DocumentName",
		"Ascii",
		"Processed date %d-%02d-%02d, batch %d, slide %d"}
	usercomment_fmt = exiv2_cmd{
		"Exif.Photo.UserComment",
		"comment charset=Ascii",
		"%s"}
)

// The work pipeline

type Image_job struct {
	src   string // path to source image file
	dest  string
	slide int
}

var queue chan Image_job

// goroutine join

var pcount = 0
var done chan int

var this_year string
var orig_year, orig_month, orig_day int

func verifyDirectory(dpath string) (err error) {
	var stat os.FileInfo
	if stat, err = os.Stat(dpath); err != nil {
		return err
	}
	if stat.IsDir() == false {
		return fmt.Errorf(ErrNotADirectory.Error(), dpath)
	}
	return nil
}

func scandir(dirname, suffix string) (file_list []string, err error) {

	var stat os.FileInfo
	var dh *os.File
	var files []os.FileInfo

	if stat, err = os.Stat(dirname); err != nil {
		return file_list, err
	}

	if stat.IsDir() == false {
		return file_list, fmt.Errorf(ErrNotADirectory.Error(), dirname)
	}

	if dh, err = os.Open(dirname); err != nil {
		return file_list, err
	}

	defer dh.Close()

	if files, err = dh.Readdir(-1); err != nil {
		return file_list, err
	}
	for _, file := range files {
		if !file.Mode().IsRegular() ||
			!strings.HasSuffix(strings.ToLower(file.Name()),
				"."+suffix) {
			continue
		}
		name := dirname + Ps + file.Name()
		file_list = append(file_list, name)
	}
	sort.Strings(file_list)
	return file_list, nil
}

func args2slides(args []string) []int {
	var sl []int

	for i := 0; i < len(args); i++ {
		toks := strings.Split(args[i], "-")
		if len(toks) == 1 {
			slide, e := strconv.ParseInt(toks[0], 10, 0)
			if e != nil {
				fmt.Printf("%s is not a number: %v\n",
					toks[0], e)
				continue
			}
			sl = append(sl, int(slide))
		} else if len(toks) == 2 {
			start, e := strconv.ParseInt(toks[0], 10, 0)
			if e != nil {
				fmt.Printf("%s is not a number: %v\n",
					toks[0], e)
				continue
			}
			end, e := strconv.ParseInt(toks[1], 10, 0)
			if e != nil {
				fmt.Printf("%s is not a number: %v\n",
					toks[1], e)
				continue
			}
			for j := start; j <= end; j++ {
				sl = append(sl, int(j))
			}
		} else {
			fmt.Printf("Error: %s is not a number or range\n",
				args[i])
		}
	}
	return sl
}
func copyFile(src string, dst string) (err error) {
	var input *os.File
	var output *os.File

	if input, err = os.Open(src); err != nil {
		return err
	}
	defer input.Close()

	if output, err = os.Create(dst); err != nil {
		return err
	}
	defer output.Close()

	_, err = io.Copy(output, input)

	return err
}

func write_cmd(tf *os.File, cmd exiv2_cmd, tag_val string) (err error) {
	cmd_line := fmt.Sprintf("set %s %s %s\n",
		cmd.tag, cmd.tag_type, tag_val)
	if _, err = tf.WriteString(cmd_line); err != nil {
		return err
	}
	return nil
}

func set_exif_tags(file string, slide int) (err error) {
	var tf *os.File
	tagcmds := "/tmp/" + strings.TrimSuffix(path.Base(file), ".dng") + ".cmds"

	if tf, err = os.Create(tagcmds); err != nil {
		return err
	}
	defer func() {
		tf.Close()
		os.Remove(tagcmds)
	}()

	tag_val := fmt.Sprintf(copyright_fmt.value, this_year, *photographer)
	if err = write_cmd(tf, copyright_fmt, tag_val); err != nil {
		return err
	}
	tag_val = fmt.Sprintf(artist_fmt.value, *photographer)
	if err = write_cmd(tf, artist_fmt, tag_val); err != nil {
		return err
	}
	tag_val = fmt.Sprintf(origdate_fmt.value, orig_year, orig_month, orig_day)
	if err = write_cmd(tf, origdate_fmt, tag_val); err != nil {
		return err
	}
	tag_val = fmt.Sprintf(docname_fmt.value,
		orig_year, orig_month, orig_day, *batch, slide)
	if err = write_cmd(tf, docname_fmt, tag_val); err != nil {
		return err
	}
	if len(*description) > 0 {
		tag_val = fmt.Sprintf(desc_fmt.value, *description)
		if err = write_cmd(tf, desc_fmt, tag_val); err != nil {
			return err
		}
	}
	if len(*comment) > 0 {
		tag_val = fmt.Sprintf(usercomment_fmt.value, *comment)
		if err = write_cmd(tf, usercomment_fmt, tag_val); err != nil {
			return err
		}
	}

	exiv2 := exec.Command("exiv2", "-m", tagcmds, file)
	if err = exiv2.Run(); err != nil {
		return err
	}
	return nil
}

func make_jpeg(infile string) (err error) {

	infilebase := strings.TrimSuffix(infile, ".dng")
	exv_file := infilebase + ".exv"
	jpeg_file := infilebase + ".jpeg"

	// dcraw -c infile | convert - jpeg_file
	dcraw := exec.Command("dcraw", "-c", infile)
	convert := exec.Command("convert", "-", jpeg_file)
	convert.Stdin, err = dcraw.StdoutPipe()
	if err != nil {
		return err
	}

	if err = convert.Start(); err != nil {
		log.Printf("Start of convert to %s failed because %s",
			jpeg_file, err.Error())
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(jpeg_file)
		}
	}()

	if err = dcraw.Run(); err != nil {
		log.Printf("Run of dcraw of %s failed because %s",
			infile, err.Error())
		return err
	}
	if err = convert.Wait(); err != nil {
		log.Printf("Convert of %s failed because %s",
			jpeg_file, err.Error())
		return err
	}

	// get the EXIF bits from the .dng
	exiv2 := exec.Command("exiv2", "ex", infile)

	if err = exiv2.Run(); err != nil {
		log.Printf("exiv2 ex of %s failed because %s",
			infile, err.Error())
		return err
	}
	defer os.Remove(exv_file)

	// And add in the EXIF bits from the source
	exiv2 = exec.Command("exiv2", "in", jpeg_file)

	if err = exiv2.Run(); err != nil {
		log.Printf("exiv2 in of %s failed because %s",
			jpeg_file, err.Error())
		return err
	}
	return nil
}

func do_work() {
	var job Image_job
	var a_job bool
	var err error

	defer func() {
		done <- 1
	}()
	for {
		job, a_job = <-queue
		if !a_job {
			break
		}
		dest_file := job.dest + "." + *file_suffix
		if err = copyFile(job.src, dest_file); err != nil {
			log.Printf("Failed to copy file %s", job.src)
			break
		}
		defer func() {
			if err != nil {
				os.Remove(dest_file)
			}
		}()
		if err = set_exif_tags(dest_file, job.slide); err != nil {
			log.Printf("Failed to set tags in file %s because %s",
				dest_file, err.Error())
			break
		}
		if *create_jpeg {
			if err = make_jpeg(dest_file); err != nil {
				log.Printf("Failed to generate JPEG file for %s",
					dest_file)
				break
			}
		}
		if *show_progress {
			fmt.Printf(".")
		}
	}
}

func main() {
	var e error
	now_time := time.Now()
	var mon time.Month

	getopt.Parse()
	if len(os.Args) < 2 || *optHelp {
		getopt.Usage()
		os.Exit(0)
	}

	this_year = fmt.Sprintf("%4d", now_time.Year())
	batch_time, e := time.Parse("Jan-2006", *orig_date)
	if e != nil {
		batch_time, e = time.Parse("Jan 2, 2006", *orig_date)
	}
	if e != nil {
		log.Printf("Unrecognized date: %s\n", *orig_date)
		os.Exit(1)
	}
	orig_year, mon, orig_day = batch_time.Date()
	orig_month = int(mon)
	dest_basename := fmt.Sprintf("Scn-%s%d%02d%02d-%02d-",
		*film_type, orig_year, orig_month, orig_day, *batch)
	e = verifyDirectory(*dst_dir)
	if e != nil {
		log.Println(e.Error())
		os.Exit(1)
	}
	slides := args2slides(getopt.Args())
	files, e := scandir(*src_dir, strings.ToLower(*file_suffix))
	if e != nil {
		log.Println(e.Error())
		os.Exit(1)
	}
	if len(slides) != len(files) {
		log.Printf("%d files found but %d slide numbers specified\n",
			len(files), len(slides))
		os.Exit(1)
	}
	fmt.Printf("Processing %d files from %s to %s with %d workers\n",
		len(slides), *src_dir, *dst_dir, *max_procs)
	done = make(chan int, *max_procs)
	queue = make(chan Image_job, *max_procs)
	for i := 0; i < *max_procs; i++ {
		go do_work()
	}
	for i := 0; i < len(slides); i++ {
		job := new(Image_job)
		job.src = files[i]
		job.dest = fmt.Sprintf("%s%s%s%02d", *dst_dir, Ps,
			dest_basename, slides[i])
		job.slide = slides[i]
		queue <- *job
	}
	close(queue)

	// Now join the workers
	for i := 0; i < *max_procs; i++ {
		<-done
	}
	fmt.Println("Done...")
}
