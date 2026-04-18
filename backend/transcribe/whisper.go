package transcribe

/*
#include <whisper.h>
#include <stdlib.h>

#cgo CFLAGS: -I.
#cgo darwin LDFLAGS: -L. -lwhisper -lggml -lggml-base -lggml-cpu -lggml-metal -lggml-blas -framework Accelerate -framework Metal -framework MetalKit -framework Foundation -framework CoreGraphics -lstdc++ -lm
#cgo linux LDFLAGS: -L. -lwhisper -lggml -lggml-base -lggml-cpu -lgomp -lstdc++ -lm
*/
import "C"

import (
	"fmt"
	"strings"
	"unsafe"
)

type whisperContext struct {
	ctx *C.struct_whisper_context
}

func whisperInit(modelPath string) (*whisperContext, error) {
	cPath := C.CString(modelPath)
	defer C.free(unsafe.Pointer(cPath))

	params := C.whisper_context_default_params()
	ctx := C.whisper_init_from_file_with_params(cPath, params)
	if ctx == nil {
		return nil, fmt.Errorf("failed to load whisper model: %s", modelPath)
	}
	return &whisperContext{ctx: ctx}, nil
}

func (w *whisperContext) Close() {
	if w.ctx != nil {
		C.whisper_free(w.ctx)
		w.ctx = nil
	}
}

// TranscribeResult holds the transcription text and detected language.
type TranscribeResult struct {
	Text     string
	Language string
}

// Transcribe runs Whisper inference on mono float32 PCM at 16kHz.
func (w *whisperContext) Transcribe(samples []float32) (TranscribeResult, error) {
	if len(samples) == 0 {
		return TranscribeResult{}, fmt.Errorf("empty audio")
	}

	params := C.whisper_full_default_params(C.WHISPER_SAMPLING_GREEDY)
	params.print_progress = C.bool(false)
	params.print_special = C.bool(false)
	params.print_realtime = C.bool(false)
	params.print_timestamps = C.bool(false)
	params.single_segment = C.bool(false)
	params.language = nil // auto-detect

	ret := C.whisper_full(w.ctx, params, (*C.float)(unsafe.Pointer(&samples[0])), C.int(len(samples)))
	if ret != 0 {
		return TranscribeResult{}, fmt.Errorf("whisper_full failed: %d", ret)
	}

	nSegments := int(C.whisper_full_n_segments(w.ctx))
	var sb strings.Builder
	for i := 0; i < nSegments; i++ {
		text := C.GoString(C.whisper_full_get_segment_text(w.ctx, C.int(i)))
		sb.WriteString(text)
	}

	langID := C.whisper_full_lang_id(w.ctx)
	lang := C.GoString(C.whisper_lang_str(langID))

	return TranscribeResult{
		Text:     strings.TrimSpace(sb.String()),
		Language: lang,
	}, nil
}
