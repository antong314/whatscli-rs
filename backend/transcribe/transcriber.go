package transcribe

import (
	"fmt"
	"sync"
)

// Transcriber manages the Whisper model and provides speech-to-text.
type Transcriber struct {
	wctx  *whisperContext
	mu    sync.Mutex
	ready bool
}

func New() *Transcriber {
	return &Transcriber{}
}

// Init downloads the model if needed and loads it.
func (t *Transcriber) Init(modelPath string, progress ProgressFunc) error {
	if modelPath == "" {
		modelPath = GetDefaultModelPath()
	}
	if !ModelExists(modelPath) {
		if err := DownloadModel(modelPath, progress); err != nil {
			return err
		}
	}
	wctx, err := whisperInit(modelPath)
	if err != nil {
		return err
	}
	t.wctx = wctx
	t.ready = true
	return nil
}

func (t *Transcriber) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ready = false
	if t.wctx != nil {
		t.wctx.Close()
		t.wctx = nil
	}
}

func (t *Transcriber) IsReady() bool {
	return t.ready
}

// TranscribeAudio decodes OGG/Opus audio data and returns the transcription.
func (t *Transcriber) TranscribeAudio(audioData []byte) (TranscribeResult, error) {
	if !t.ready {
		return TranscribeResult{}, fmt.Errorf("transcriber not initialized")
	}

	samples, err := DecodeAudioToFloat32(audioData)
	if err != nil {
		return TranscribeResult{}, fmt.Errorf("decode audio: %w", err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	return t.wctx.Transcribe(samples)
}
