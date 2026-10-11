package media

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Bodokh/my-whatsapp-mcp/internal/archive"
	"github.com/Bodokh/my-whatsapp-mcp/internal/compute"
)

const voiceMaximumBytes int64 = 25 << 20
const maximumPCMBytes int64 = 20 * 60 * 16000 * 2
const slicePCMBytes = 30 * 16000 * 2

type audioSlice struct {
	StartMS, EndMS int64
	WAV            []byte
	Silent         bool
}

func silentPCM(b []byte) bool {
	// Only exact digital silence is omitted. Low-volume speech is not guessed
	// silent; VAD/quality are separately measured profile capabilities.
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
func walkPCMSlices(r io.Reader, size int64, visit func(audioSlice) error) error {
	if size > maximumPCMBytes {
		return ErrLimit
	}
	if size < 0 || size%2 != 0 {
		return ErrCorrupt
	}
	if size == 0 {
		return ErrCorrupt
	}
	for offset := int64(0); offset < size; offset += slicePCMBytes {
		n := int(min(int64(slicePCMBytes), size-offset))
		wav := make([]byte, n+44)
		copy(wav[:4], "RIFF")
		binary.LittleEndian.PutUint32(wav[4:8], uint32(n+36))
		copy(wav[8:16], "WAVEfmt ")
		binary.LittleEndian.PutUint32(wav[16:20], 16)
		binary.LittleEndian.PutUint16(wav[20:22], 1)
		binary.LittleEndian.PutUint16(wav[22:24], 1)
		binary.LittleEndian.PutUint32(wav[24:28], 16000)
		binary.LittleEndian.PutUint32(wav[28:32], 32000)
		binary.LittleEndian.PutUint16(wav[32:34], 2)
		binary.LittleEndian.PutUint16(wav[34:36], 16)
		copy(wav[36:40], "data")
		binary.LittleEndian.PutUint32(wav[40:44], uint32(n))
		if _, err := io.ReadFull(r, wav[44:]); err != nil {
			return ErrCorrupt
		}
		s := audioSlice{StartMS: offset / 32, EndMS: (offset + int64(n) + 31) / 32, WAV: wav, Silent: silentPCM(wav[44:])}
		if err := visit(s); err != nil {
			return err
		}
	}
	return nil
}

func decoderArguments(format string) ([]string, error) {
	demux := ""
	switch format {
	case "ogg", "opus":
		demux = "ogg"
	case "wav":
		demux = "wav"
	default:
		return nil, ErrUnsupported
	}
	// Input is an inherited descriptor only; demuxer selection prevents embedded
	// playlists from invoking an unrelated decoder or URL protocol.
	return []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-max_alloc", "67108864", "-threads", "1", "-filter_threads", "1", "-filter_complex_threads", "1", "-protocol_whitelist", "pipe", "-probesize", "1048576", "-analyzeduration", "2000000", "-f", demux, "-i", "pipe:4", "-map", "0:a:0", "-vn", "-sn", "-dn", "-ac", "1", "-ar", "16000", "-threads", "1", "-f", "s16le", "pipe:1"}, nil
}

func (e *Extractor) transcribe(ctx context.Context, job extractionInput, original *Original, guard *compute.SchedulerLease, own *extractionOwnership, lease MediaModelLease) (extractionOutput, error) {
	meta := original.Metadata()
	if meta.Bytes > voiceMaximumBytes {
		return extractionOutput{}, ErrLimit
	}
	args, err := decoderArguments(meta.Format)
	if err != nil {
		return extractionOutput{}, err
	}
	pcm, err := e.options.Store.ReserveDecoded(ctx, meta.Source, meta.MediaID, maximumPCMBytes)
	if err != nil {
		return extractionOutput{}, err
	}
	e.trackDecoded(ctx, own, pcm)
	input, err := original.Open()
	if err != nil {
		return extractionOutput{}, err
	}
	own.add(input.Close)
	if err = e.addWorkerGuards(own, original, nil, nil); err != nil {
		return extractionOutput{}, err
	}
	if err = e.run(ctx, e.options.Runtimes.Decoder, args, []*os.File{input}, pcm.File(), maximumPCMBytes, own); err != nil {
		return extractionOutput{}, err
	}
	if err = pcm.Seal(ctx); err != nil {
		return extractionOutput{}, err
	}
	f, err := pcm.Open()
	if err != nil {
		return extractionOutput{}, err
	}
	own.add(f.Close)
	st, err := f.Stat()
	if err != nil {
		return extractionOutput{}, ErrFailed
	}
	var documents []archive.Document
	var outputBytes int
	err = walkPCMSlices(io.NewSectionReader(f, 0, st.Size()), st.Size(), func(slice audioSlice) (sliceErr error) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := pcm.Renew(ctx); err != nil {
			return err
		}
		if slice.Silent {
			return nil
		}
		_, lang, err := e.speechRoute(slice.StartMS, slice.EndMS)
		if err != nil {
			return err
		}
		// Each native process unloads its model before checkpointing. The immutable
		// installed-profile lease stays pinned, without keeping a model resident.
		scope := newOwnership()
		scope.cancel = own.cancel
		defer func() {
			if err := scope.finish(); err != nil {
				own.pending = scope.done
				sliceErr = err
			}
		}()
		model, err := lease.Open("model")
		if err != nil {
			return ErrModelMissing
		}
		scope.add(model.Close)
		wav, err := e.options.Store.ReserveDecoded(ctx, meta.Source, meta.MediaID, int64(len(slice.WAV)))
		if err != nil {
			return err
		}
		e.trackDecoded(ctx, scope, wav)
		if _, err = wav.File().Write(slice.WAV); err != nil {
			return err
		}
		if err = wav.Seal(ctx); err != nil {
			return err
		}
		audio, err := wav.Open()
		if err != nil {
			return err
		}
		scope.add(audio.Close)
		var text strings.Builder
		args := []string{"--model", "/dev/fd/5", "--file", "/dev/fd/4", "--threads", "2", "--processors", "1", "--language", lang, "--max-context", "0", "--no-gpu", "--no-prints", "--no-timestamps", "--no-fallback", "--suppress-nst"}
		if err = e.addWorkerGuards(scope, original, lease, wav); err != nil {
			return err
		}
		if err = e.run(ctx, e.options.Runtimes.Speech, args, []*os.File{audio, model}, &text, maxGeneratedBytes, scope); err != nil {
			return err
		}
		value, err := generatedText([]byte(text.String()))
		if err != nil {
			return err
		}
		if value == "" {
			return ErrFailed
		}
		outputBytes += len(value)
		if outputBytes > maxGeneratedBytes {
			return ErrLimit
		}
		documents = append(documents, e.resultDocuments(job, meta, archive.DocumentTranscript, value, lease.Profile().Identity, slice.StartMS, slice.EndMS, strconv.FormatInt(slice.StartMS, 10))...)
		if err = scope.finish(); err != nil {
			return err
		}
		if err = guard.Checkpoint(); err != nil {
			return err
		}
		if e.options.BeforeHeavy != nil {
			return e.options.BeforeHeavy(ctx)
		}
		return nil
	})
	if err != nil {
		return extractionOutput{}, err
	}
	if len(documents) == 0 {
		return extractionOutput{outcome: archive.ExtractionSilent}, nil
	}
	return extractionOutput{documents: documents, outcome: archive.ExtractionText}, nil
}
