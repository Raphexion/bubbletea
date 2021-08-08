// +build solaris

// nolint:revive
package tea

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

var selectMaxFd = 1024

// newCancelReader returns a reader and a cancel function. If the input reader
// is an *os.File, the cancel function can be used to interrupt a blocking call
// read call. In this case, the cancel function returns true if the call was
// cancelled successfully. If the input reader is not a *os.File or the file
// descriptor is 1024 or larger, the cancel function does nothing and always
// returns false. The generic unix implementation is based on the posix select
// syscall.
func newCancelReader(reader io.Reader) (cancelReader, error) {
	file, ok := reader.(*os.File)
	if !ok || file.Fd() >= uintptr(selectMaxFd) {
		return newFallbackCancelReader(reader)
	}
	r := &selectCancelReader{file: file}

	var err error

	r.cancelSignalReader, r.cancelSignalWriter, err = os.Pipe()
	if err != nil {
		return nil, err
	}

	return r, nil
}

type selectCancelReader struct {
	file               *os.File
	cancelSignalReader *os.File
	cancelSignalWriter *os.File
	cancelled          bool
}

func (r *selectCancelReader) Read(data []byte) (int, error) {
	if r.cancelled {
		return 0, errCanceled
	}

	for {
		err := waitForRead(r.file, r.cancelSignalReader)
		if err != nil {
			if errors.Is(err, unix.EINTR) && !r.cancelled {
				continue // try again if syscall was interrupted
			}

			if errors.Is(err, errCanceled) {
				// remove signal from pipe
				var b [1]byte
				_, readErr := r.cancelSignalReader.Read(b[:])
				if readErr != nil {
					return 0, fmt.Errorf("reading cancel signal: %w", readErr)
				}
			}

			return 0, err
		}

		return r.file.Read(data)
	}
}

func (r *selectCancelReader) Cancel() bool {
	r.cancelled = true

	// send cancel signal
	_, err := r.cancelSignalWriter.Write([]byte{'c'})
	if err != nil {
		return false
	}

	return true
}

func (r *selectCancelReader) Close() error {
	var errMsgs []string

	// close pipe
	err := r.cancelSignalWriter.Close()
	if err != nil {
		errMsgs = append(errMsgs, fmt.Sprintf("closing cancel signal writer: %v", err))
	}

	err = r.cancelSignalReader.Close()
	if err != nil {
		errMsgs = append(errMsgs, fmt.Sprintf("closing cancel signal reader: %v", err))
	}

	if len(errMsgs) > 0 {
		return fmt.Errorf(strings.Join(errMsgs, ", "))
	}

	return nil
}

func waitForRead(reader *os.File, abort *os.File) error {
	readerFd := int(reader.Fd())
	abortFd := int(abort.Fd())

	maxFd := readerFd
	if abortFd > maxFd {
		maxFd = abortFd
	}

	// this is a limitation of the select syscall
	if maxFd >= selectMaxFd {
		return fmt.Errorf("cannot select on file descriptor %d which is larger than 1024", maxFd)
	}

	fdSet := &unix.FdSet{}
	fdSet.Set(int(reader.Fd()))
	fdSet.Set(int(abort.Fd()))

	_, err := unix.Select(maxFd+1, fdSet, nil, nil, nil)
	if err != nil {
		return fmt.Errorf("select: %w", err)
	}

	if fdSet.IsSet(abortFd) {
		return errCanceled
	}

	if fdSet.IsSet(readerFd) {
		return nil
	}

	return fmt.Errorf("select returned without setting a file descriptor")
}
