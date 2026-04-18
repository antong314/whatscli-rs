package transcribe

import (
	"bytes"
	"fmt"
	"io"

	"gopkg.in/hraban/opus.v2"
)

const (
	whisperSampleRate = 16000
	opusSampleRate    = 48000
	opusChannels      = 1
	// Downsample ratio from 48kHz to 16kHz.
	downsampleRatio = opusSampleRate / whisperSampleRate
)

// DecodeAudioToFloat32 decodes OGG/Opus audio bytes to mono float32 PCM at
// 16kHz suitable for Whisper. Uses libopusfile via hraban/opus for reliable
// decoding, then downsamples from 48kHz to 16kHz with simple averaging.
func DecodeAudioToFloat32(data []byte) ([]float32, error) {
	stream, err := opus.NewStream(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open opus stream: %w", err)
	}
	defer stream.Close()

	// libopusfile always decodes to 48kHz. Read in chunks of int16 PCM.
	const chunkSamples = 48000
	buf := make([]int16, chunkSamples)
	var all48k []int16

	for {
		n, readErr := stream.Read(buf)
		if n > 0 {
			all48k = append(all48k, buf[:n]...)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read opus stream: %w", readErr)
		}
	}

	if len(all48k) == 0 {
		return nil, fmt.Errorf("no audio decoded")
	}

	return downsample48to16(all48k), nil
}

// downsample48to16 converts 48kHz int16 mono to 16kHz float32 mono by
// averaging every 3 consecutive samples.
func downsample48to16(samples []int16) []float32 {
	outLen := len(samples) / downsampleRatio
	out := make([]float32, outLen)
	for i := 0; i < outLen; i++ {
		var sum int32
		base := i * downsampleRatio
		for j := 0; j < downsampleRatio; j++ {
			sum += int32(samples[base+j])
		}
		out[i] = float32(sum/int32(downsampleRatio)) / 32768.0
	}
	return out
}
