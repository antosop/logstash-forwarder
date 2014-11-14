package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os" // for File and friends
	"time"
)

type Harvester struct {
	IsTracked  bool
	Path       string /* the file path to harvest */
	FileConfig FileConfig
	Offset     int64
	FinishChan chan int64

	file *os.File /* the file being watched */
	size int64
	info *os.FileInfo /* most recent size of the file being watched */
}

var FILE_TRUNCATED = errors.New("file has been truncated")

func (h *Harvester) Harvest(output chan *FileEvent) {

	// On completion, push offset so we can continue where we left off if we relaunch on the same file
	defer func() { h.FinishChan <- h.Offset }()

	var line uint64 = 0 // Ask registrar about the line number

	if h.IsTracked {
		emit("harvest: %q position:%d\n", h.Path, h.Offset)
	} else if options.tailOnRotate {
		emit("harvest: (tailing) %q\n", h.Path)
	} else {
		emit("harvest: %q\n", h.Path)
	}

	buffer := new(bytes.Buffer)

	var read_timeout = 10 * time.Second
	last_read_time := time.Now()
	for {
		text, bytesread, err := h.readline(buffer, read_timeout)

		if err != nil {
			if err == FILE_TRUNCATED {
				emit("File truncated, seeking to beginning: %s\n", h.Path)
				h.Offset = 0
				continue
			} else if err == io.EOF {
				// timed out waiting for data, got eof.
				if age := time.Since(last_read_time); age > h.FileConfig.deadtime {
					// if last_read_time was more than dead time, this file is probably
					// dead. Stop watching it.
					emit("Stopping harvest of %s; last change was %v ago\n", h.Path, age)
					return
				}
				continue
			} else {
				emit("Unexpected state reading from %s; error: %s\n", h.Path, err)
				return
			}
		}
		last_read_time = time.Now()

		line++
		event := &FileEvent{
			Source:   &h.Path,
			Offset:   h.Offset,
			Line:     line,
			Text:     text,
			Fields:   &h.FileConfig.Fields,
			fileinfo: h.info,
		}
		h.Offset += int64(bytesread)

		output <- event // ship the new event downstream
	} /* forever */
}

func (h *Harvester) open() *os.File {
	// Special handling that "-" means to read from standard input
	if h.Path == "-" {
		h.file = os.Stdin
		return h.file
	}

	for {
		var err error
		h.file, err = os.Open(h.Path)

		if err != nil {
			// retry on failure.
			emit("Failed opening %s: %s\n", h.Path, err)
			time.Sleep(5 * time.Second)
		} else {
			break
		}
	}

	// Check we are not following a rabbit hole (symlinks, etc.)
	mustBeRegularFile(h.file) // panics

	if h.IsTracked {
		h.file.Seek(h.Offset, os.SEEK_SET)
	} else if options.tailOnRotate {
		h.Offset, _ = h.file.Seek(0, os.SEEK_END)
		h.IsTracked = true
	} else {
		h.Offset, _ = h.file.Seek(0, os.SEEK_SET)
		h.IsTracked = true
	}

	return h.file
}

func (h *Harvester) readline(buffer *bytes.Buffer, eof_timeout time.Duration) (*string, int, error) {
	var is_partial bool = true
	var newline_length int = 1
	start_time := time.Now()

	for {
		segment, err := h.read(int64(buffer.Len()))

		if segment != nil && len(segment) > 0 {
			if segment[len(segment)-1] == '\n' {
				// Found a complete line
				is_partial = false

				// Check if also a CR present
				if len(segment) > 1 && segment[len(segment)-2] == '\r' {
					newline_length++
				}
			}

			// TODO(sissel): if buffer exceeds a certain length, maybe report an error condition? chop it?
			buffer.Write(segment)
		}

		if err != nil {
			if err == io.EOF && is_partial {
				time.Sleep(1 * time.Second) // TODO(sissel): Implement backoff

				// Give up waiting for data after a certain amount of time.
				// If we time out, return the error (eof)
				if time.Since(start_time) > eof_timeout {
					return nil, 0, err
				}
				continue
			} else {
				emit("error: Harvester.readLine: %s", err.Error())
				return nil, 0, err // TODO(sissel): don't do this?
			}
		}

		// If we got a full line, return the whole line without the EOL chars (CRLF or LF)
		if !is_partial {
			// Get the str length with the EOL chars (LF or CRLF)
			bufferSize := buffer.Len()
			str := new(string)
			*str = buffer.String()[:bufferSize-newline_length]
			// Reset the buffer for the next line
			buffer.Reset()
			return str, bufferSize, nil
		}
	} /* forever read chunks */

	return nil, 0, nil
}

func (h *Harvester) read(offset int64) (line []byte, err error) {
	h.open()
	defer h.file.Close()

	info, _ := h.file.Stat()
	defer func(size int64, info *os.FileInfo) {
		h.size = size
		h.info = info
	}(info.Size(), &info)
	if info.Size() < h.size {
		return nil, FILE_TRUNCATED
	}

	h.file.Seek(offset, os.SEEK_CUR)
	reader := bufio.NewReader(h.file)

	return reader.ReadBytes('\n')
}

// panics
func mustBeRegularFile(f *os.File) {
	if f == nil {
		panic(fmt.Errorf("Harvester: BUG: f arg is nil"))
	}
	info, e := f.Stat()
	if e != nil {
		panic(fmt.Errorf("Harvester: FAULT: stat error: %s", e.Error()))
	}

	if !info.Mode().IsRegular() {
		panic(fmt.Errorf("Harvester: not a regular file:%q", info.Mode(), info.Name()))
	}
}
