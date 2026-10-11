package workerlife

import (
	"io"
	"strconv"
	"strings"
)

const (
	// HolderModeArgument is a private self-reexec entry point, not a public CLI.
	HolderModeArgument  = "--workerlife-holder"
	maxHolderFrameBytes = 256
	maxHolderGuards     = 8
)

// readHolderFrame consumes one bounded ASCII frame and removes its newline.
// It never buffers bytes from the next frame or allocates in proportion to input.
func readHolderFrame(reader io.Reader) (string, error) {
	var frame [maxHolderFrameBytes]byte
	var next [1]byte
	for index := range frame {
		n, err := reader.Read(next[:])
		if n != 1 || err != nil {
			return "", ErrProtocol
		}
		if next[0] == '\n' {
			if index == 0 {
				return "", ErrProtocol
			}
			return string(frame[:index]), nil
		}
		if next[0] < ' ' || next[0] > '~' {
			return "", ErrProtocol
		}
		frame[index] = next[0]
	}
	return "", ErrLimit
}

// writeHolderFrame accepts the frame without its newline and handles short writes.
func writeHolderFrame(writer io.Writer, frame string) error {
	if len(frame) == 0 || len(frame) >= maxHolderFrameBytes {
		return ErrLimit
	}
	for index := range frame {
		if frame[index] < ' ' || frame[index] > '~' {
			return ErrProtocol
		}
	}
	remaining := []byte(frame + "\n")
	for len(remaining) > 0 {
		n, err := writer.Write(remaining)
		if n < 0 || n > len(remaining) || err != nil || n == 0 {
			return ErrProtocol
		}
		remaining = remaining[n:]
	}
	return nil
}

func parseInit(frame string) (int, error) {
	if !strings.HasPrefix(frame, "INIT ") {
		return 0, ErrProtocol
	}
	count, err := holderDecimal(frame[len("INIT "):])
	if err != nil {
		return 0, err
	}
	if count > maxHolderGuards {
		return 0, ErrLimit
	}
	return count, nil
}

// parseRegistration returns noop=true only for the exact NOOP frame.
func parseRegistration(frame string) (pid int, noop bool, err error) {
	if frame == "NOOP" {
		return 0, true, nil
	}
	if !strings.HasPrefix(frame, "GROUP ") {
		return 0, false, ErrProtocol
	}
	pid, err = holderDecimal(frame[len("GROUP "):])
	if err != nil || pid == 0 {
		return 0, false, ErrProtocol
	}
	return pid, false, nil
}

func holderDecimal(token string) (int, error) {
	if len(token) == 0 || (len(token) > 1 && token[0] == '0') {
		return 0, ErrProtocol
	}
	for index := range token {
		if token[index] < '0' || token[index] > '9' {
			return 0, ErrProtocol
		}
	}
	value, err := strconv.ParseInt(token, 10, 32)
	if err != nil {
		return 0, ErrProtocol
	}
	return int(value), nil
}
