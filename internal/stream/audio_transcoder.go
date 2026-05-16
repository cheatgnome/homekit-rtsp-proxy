package stream

/*
#cgo LDFLAGS: -lfdk-aac
#include <fdk-aac/aacdecoder_lib.h>
#include <fdk-aac/aacenc_lib.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>

// Configure decoder with raw AudioSpecificConfig.
// Wraps aacDecoder_ConfigRaw to avoid Go pointer issues with UCHAR**.
static AAC_DECODER_ERROR config_raw(HANDLE_AACDECODER dec, unsigned char *conf, unsigned int confLen) {
    UCHAR *confArray[1] = { conf };
    UINT   lenArray[1]  = { confLen };
    return aacDecoder_ConfigRaw(dec, confArray, lenArray);
}

// Get decoder stream info as a string for diagnostics.
static void get_stream_info(HANDLE_AACDECODER dec, char *buf, int bufLen) {
    CStreamInfo *info = aacDecoder_GetStreamInfo(dec);
    if (info == NULL) {
        snprintf(buf, bufLen, "null");
        return;
    }
    snprintf(buf, bufLen, "aot=%d sampleRate=%d frameSize=%d numChannels=%d flags=0x%x",
             info->aot, info->sampleRate, info->frameSize, info->numChannels, info->flags);
}

// Decode one AAC frame to PCM.
// Returns number of decoded samples on success, negative on error.
static int decode_frame(HANDLE_AACDECODER dec, unsigned char *in, int inLen, INT_PCM *out, int outLen) {
    UINT bytesValid = (UINT)inLen;
    UCHAR *inBuf = in;
    UINT bufSize = (UINT)inLen;

    AAC_DECODER_ERROR err = aacDecoder_Fill(dec, &inBuf, &bufSize, &bytesValid);
    if (err != AAC_DEC_OK) {
        return -1;
    }

    err = aacDecoder_DecodeFrame(dec, out, outLen, 0);
    if (err != AAC_DEC_OK) {
        return -2;
    }

    CStreamInfo *info = aacDecoder_GetStreamInfo(dec);
    if (info == NULL) {
        return -3;
    }
    return info->frameSize * info->numChannels;
}

// Dump PCM samples directly from C to a file, bypassing Go memory.
static void dump_pcm_c(INT_PCM *buf, int nSamples, const char *path) {
    FILE *f = fopen(path, "ab");
    if (!f) return;
    fwrite(buf, sizeof(INT_PCM), nSamples, f);
    fclose(f);
}

// Print first 8 PCM samples for diagnostics.
static void print_pcm_samples(INT_PCM *buf, int nSamples, int frameNum) {
    int n = nSamples < 8 ? nSamples : 8;
    printf("[pcm_raw] frame=%d nSamples=%d sizeof(INT_PCM)=%d first8:", frameNum, nSamples, (int)sizeof(INT_PCM));
    for (int i = 0; i < n; i++) {
        printf(" %d", (int)buf[i]);
    }
    printf("\n");
}

// Encode one PCM frame to AAC-LC.
// Returns encoded size on success, negative on error.
static int encode_frame(HANDLE_AACENCODER enc, INT_PCM *in, int inSamples, unsigned char *out, int outLen) {
    AACENC_BufDesc inBufDesc  = {0};
    AACENC_BufDesc outBufDesc = {0};
    AACENC_InArgs  inArgs     = {0};
    AACENC_OutArgs outArgs    = {0};

    // Input buffer setup
    int inBufId       = IN_AUDIO_DATA;
    int inBufSize     = inSamples * (int)sizeof(INT_PCM);
    int inBufElSize   = (int)sizeof(INT_PCM);
    void *inPtr       = (void *)in;

    inBufDesc.numBufs           = 1;
    inBufDesc.bufs              = &inPtr;
    inBufDesc.bufferIdentifiers = &inBufId;
    inBufDesc.bufSizes          = &inBufSize;
    inBufDesc.bufElSizes        = &inBufElSize;

    // Output buffer setup
    int outBufId      = OUT_BITSTREAM_DATA;
    int outBufSize    = outLen;
    int outBufElSize  = 1;
    void *outPtr      = (void *)out;

    outBufDesc.numBufs           = 1;
    outBufDesc.bufs              = &outPtr;
    outBufDesc.bufferIdentifiers = &outBufId;
    outBufDesc.bufSizes          = &outBufSize;
    outBufDesc.bufElSizes        = &outBufElSize;

    inArgs.numInSamples = inSamples;

    AACENC_ERROR err = aacEncEncode(enc, &inBufDesc, &outBufDesc, &inArgs, &outArgs);
    if (err != AACENC_OK) {
        return -1;
    }
    return outArgs.numOutBytes;
}
*/
import "C"

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
	"unsafe"
)

// AudioTranscoder transcodes AAC-ELD audio to AAC-LC using libfdk-aac.
// AAC-ELD uses 480 or 512 samples/frame; AAC-LC uses 1024. We accumulate
// decoded PCM in a ring buffer and encode whenever we have a full LC frame.
type AudioTranscoder struct {
	gain         int // PCM gain factor (0 = mute, 512 = ~54dB)
	decoder      C.HANDLE_AACDECODER
	encoder      C.HANDLE_AACENCODER
	outASC       []byte      // AAC-LC AudioSpecificConfig from encoder
	decBuf       []C.INT_PCM // temp buffer for one decoded ELD frame
	pcmRing      []C.INT_PCM // accumulator for PCM samples
	pcmLen       int         // current number of valid samples in pcmRing
	encBuf       []byte      // output buffer for encoder
	encFrameSize int         // encoder's frame size (1024 for AAC-LC)
	diagCount    int         // diagnostic counter
	frameCount   int         // total frames decoded by production decoder

	// Auto-detection: try ASC candidates on first frames to find the right one.
	ascCandidates []string
	sampleRate    int
	eldASCHex     string   // the ASC we're currently using
	detecting     bool     // true during auto-detection phase
	detectFrames  [][]byte // buffered raw frames for detection
}

// eldASCCandidates returns possible AudioSpecificConfig hex strings to try
// for AAC-ELD at the given sample rate. The camera may use plain ELD or
// ELD with LD-SBR (core at half rate), and 480 or 512 sample frames.
func eldASCCandidates(sampleRate int) []string {
	// ASC bit layout for ELD (AOT 39):
	//   bits 0-4:  11111 (escape)
	//   bits 5-10: 000111 (ext type 7 → AOT 39)
	//   bits 11-14: frequency index (8=16kHz, 11=8kHz)
	//   bits 15-18: channel config (1=mono)
	//   bit 19: frameLengthFlag (0=512, 1=480)
	//   bits 20-22: resilience flags (000)
	//   bit 23: ldSbrPresentFlag
	//
	// For 16kHz output:
	//   F8F02000 = ELD 16kHz(idx=8)  mono, 512, no SBR
	//   F8F03000 = ELD 16kHz(idx=8)  mono, 480, no SBR
	//   F8F62100 = ELD 8kHz(idx=11)  mono, 512, LD-SBR → 16kHz output
	//   F8F63100 = ELD 8kHz(idx=11)  mono, 480, LD-SBR → 16kHz output
	switch sampleRate {
	case 16000:
		return []string{
			"F8F62100", // 8kHz core + LD-SBR → 16kHz, 512 samples
			"F8F63100", // 8kHz core + LD-SBR → 16kHz, 480 samples
			"F8F02000", // 16kHz, 512 samples, no SBR
			"F8F03000", // 16kHz, 480 samples, no SBR
		}
	case 24000:
		return []string{
			"F8F22100", // 12kHz(idx=9) core + LD-SBR → 24kHz
			"F8EC2000", // 24kHz, no SBR
		}
	case 8000:
		return []string{
			"F8F82000", // 8kHz(idx=11), 512, no SBR
			"F8F83000", // 8kHz(idx=11), 480, no SBR
		}
	default:
		return []string{"F8F02000"}
	}
}

// NewAudioTranscoder creates a transcoder that decodes AAC-ELD and encodes AAC-LC.
// It tries multiple ASC configurations to find one that works with the camera's
// actual encoding format.
func NewAudioTranscoder(sampleRate int, eldASCHex string, gain int) (*AudioTranscoder, error) {
	// Run a self-test to verify FDK-AAC's ELD encoder+decoder work on this platform.
	selfTestELD(sampleRate)

	// Use the same ASC that our self-test ELD encoder produces (F8F02000 for 16kHz).
	// This is the standard AAC-ELD configuration without SBR.
	// Auto-detection found audio was present but ~60dB too quiet with SBR ASCs,
	// suggesting the camera uses plain ELD (no SBR) matching the self-test config.
	asc := eldASCHex // default from caller
	fmt.Printf("[audio_transcoder] using ASC %s for %dHz (no auto-detection)\n", asc, sampleRate)

	t, err := newTranscoderWithASC(sampleRate, asc)
	if err != nil {
		return nil, fmt.Errorf("init transcoder with ASC %s: %w", asc, err)
	}

	t.gain = gain
	t.sampleRate = sampleRate
	t.detecting = false // no auto-detection

	return t, nil
}

func newTranscoderWithASC(sampleRate int, eldASCHex string) (*AudioTranscoder, error) {
	t := &AudioTranscoder{eldASCHex: eldASCHex}

	// --- Decoder (AAC-ELD → PCM) ---
	t.decoder = C.aacDecoder_Open(C.TT_MP4_RAW, 1)
	if t.decoder == nil {
		return nil, fmt.Errorf("aacDecoder_Open failed")
	}

	// Feed AudioSpecificConfig to decoder.
	ascBytes, err := hex.DecodeString(strings.TrimSpace(eldASCHex))
	if err != nil {
		C.aacDecoder_Close(t.decoder)
		return nil, fmt.Errorf("invalid ASC hex %q: %w", eldASCHex, err)
	}

	decErr := C.config_raw(t.decoder, (*C.uchar)(unsafe.Pointer(&ascBytes[0])), C.uint(len(ascBytes)))
	if decErr != C.AAC_DEC_OK {
		C.aacDecoder_Close(t.decoder)
		return nil, fmt.Errorf("aacDecoder_ConfigRaw failed: %d", decErr)
	}

	// Disable DRC and PCM limiter — camera's bitstream may contain DRC metadata
	// that causes heavy attenuation of the decoded output.
	C.aacDecoder_SetParam(t.decoder, C.AAC_PCM_LIMITER_ENABLE, 0)
	C.aacDecoder_SetParam(t.decoder, C.AAC_DRC_REFERENCE_LEVEL, C.INT(-1))
	C.aacDecoder_SetParam(t.decoder, C.AAC_DRC_ATTENUATION_FACTOR, 0)
	C.aacDecoder_SetParam(t.decoder, C.AAC_DRC_BOOST_FACTOR, 0)

	// --- Encoder (PCM → AAC-LC) ---
	var encoder C.HANDLE_AACENCODER
	encErr := C.aacEncOpen(&encoder, 0, 1) // 1 channel
	if encErr != C.AACENC_OK {
		C.aacDecoder_Close(t.decoder)
		return nil, fmt.Errorf("aacEncOpen failed: %d", encErr)
	}
	t.encoder = encoder

	// Configure encoder.
	params := []struct {
		param C.AACENC_PARAM
		value int
	}{
		{C.AACENC_AOT, C.AOT_AAC_LC},
		{C.AACENC_SAMPLERATE, sampleRate},
		{C.AACENC_CHANNELMODE, C.MODE_1}, // mono
		{C.AACENC_BITRATE, 64000},
		{C.AACENC_TRANSMUX, C.TT_MP4_RAW}, // raw frames (we add AU headers ourselves)
	}
	for _, p := range params {
		if e := C.aacEncoder_SetParam(t.encoder, p.param, C.UINT(p.value)); e != C.AACENC_OK {
			t.Close()
			return nil, fmt.Errorf("aacEncoder_SetParam(%d) failed: %d", p.param, e)
		}
	}

	if e := C.aacEncEncode(t.encoder, nil, nil, nil, nil); e != C.AACENC_OK {
		t.Close()
		return nil, fmt.Errorf("aacEncEncode init failed: %d", e)
	}

	// Get encoder info (frame size, ASC).
	var info C.AACENC_InfoStruct
	if e := C.aacEncInfo(t.encoder, &info); e != C.AACENC_OK {
		t.Close()
		return nil, fmt.Errorf("aacEncInfo failed: %d", e)
	}

	t.encFrameSize = int(info.frameLength)

	// Extract ASC from encoder info.
	ascSize := int(info.confSize)
	t.outASC = make([]byte, ascSize)
	for i := 0; i < ascSize; i++ {
		t.outASC[i] = byte(info.confBuf[i])
	}

	// Allocate buffers.
	t.decBuf = make([]C.INT_PCM, 8192)              // one decoded ELD frame (480-512 samples, room to spare)
	t.pcmRing = make([]C.INT_PCM, t.encFrameSize*3) // accumulator (holds ~3 LC frames worth)
	t.encBuf = make([]byte, 2048)

	return t, nil
}

const detectFrameCount = 20 // number of frames to buffer for ASC auto-detection

// Transcode decodes one raw AAC-ELD frame, accumulates PCM, and encodes
// AAC-LC frames when enough samples are available. Returns nil output (no
// error) when more input is needed before a full LC frame can be produced.
func (t *AudioTranscoder) Transcode(aacELDFrame []byte) ([]byte, error) {
	if len(aacELDFrame) == 0 {
		return nil, fmt.Errorf("empty input frame")
	}

	// Auto-detection phase: buffer frames, then try each ASC candidate.
	if t.detecting {
		frameCopy := make([]byte, len(aacELDFrame))
		copy(frameCopy, aacELDFrame)
		t.detectFrames = append(t.detectFrames, frameCopy)

		if len(t.detectFrames) < detectFrameCount {
			return nil, nil // keep buffering
		}

		// We have enough frames. Try each ASC and pick the best.
		t.detecting = false
		bestASC, err := t.autoDetectASC()
		if err != nil {
			fmt.Printf("[audio_transcoder] auto-detect failed: %v, keeping %s\n", err, t.eldASCHex)
		} else if bestASC != t.eldASCHex {
			fmt.Printf("[audio_transcoder] switching ASC from %s to %s\n", t.eldASCHex, bestASC)
			// Re-create decoder with the winning ASC.
			C.aacDecoder_Close(t.decoder)
			t.decoder = C.aacDecoder_Open(C.TT_MP4_RAW, 1)
			ascBytes, _ := hex.DecodeString(bestASC)
			C.config_raw(t.decoder, (*C.uchar)(unsafe.Pointer(&ascBytes[0])), C.uint(len(ascBytes)))
			t.eldASCHex = bestASC
		}

		// Now decode all buffered frames through the (possibly new) decoder.
		for _, frame := range t.detectFrames {
			// Ignore output during replay — just priming the decoder.
			t.transcodeFrame(frame)
		}
		t.detectFrames = nil
		return nil, nil
	}

	return t.transcodeFrame(aacELDFrame)
}

// autoDetectASC tries each ASC candidate on the buffered frames and returns
// the one that produces the highest peak PCM (i.e., real audio, not silence).
func (t *AudioTranscoder) autoDetectASC() (string, error) {
	type result struct {
		asc     string
		maxPCM  int
		decoded int
		err     error
	}

	var results []result

	for _, asc := range t.ascCandidates {
		// Create a temporary decoder with this ASC.
		dec := C.aacDecoder_Open(C.TT_MP4_RAW, 1)
		if dec == nil {
			results = append(results, result{asc: asc, err: fmt.Errorf("open failed")})
			continue
		}

		ascBytes, err := hex.DecodeString(asc)
		if err != nil {
			C.aacDecoder_Close(dec)
			results = append(results, result{asc: asc, err: err})
			continue
		}

		rc := C.config_raw(dec, (*C.uchar)(unsafe.Pointer(&ascBytes[0])), C.uint(len(ascBytes)))
		if rc != C.AAC_DEC_OK {
			C.aacDecoder_Close(dec)
			results = append(results, result{asc: asc, err: fmt.Errorf("config failed: %d", rc)})
			continue
		}

		// Decode all buffered frames and measure peak PCM.
		tmpBuf := make([]C.INT_PCM, 8192)
		var maxPCM C.INT_PCM
		decodedFrames := 0

		for _, frame := range t.detectFrames {
			ns := C.decode_frame(
				dec,
				(*C.uchar)(unsafe.Pointer(&frame[0])),
				C.int(len(frame)),
				&tmpBuf[0],
				C.int(len(tmpBuf)),
			)
			if ns <= 0 {
				continue
			}
			decodedFrames++
			for i := 0; i < int(ns); i++ {
				s := tmpBuf[i]
				if s < 0 {
					s = -s
				}
				if s > maxPCM {
					maxPCM = s
				}
			}
		}

		// Get stream info for diagnostics.
		var infoBuf [256]C.char
		C.get_stream_info(dec, &infoBuf[0], 256)
		streamInfo := C.GoString(&infoBuf[0])

		C.aacDecoder_Close(dec)
		results = append(results, result{
			asc:     asc,
			maxPCM:  int(maxPCM),
			decoded: decodedFrames,
		})
		fmt.Printf("[audio_detect] ASC=%s decoded=%d/%d maxPCM=%d info=%s\n",
			asc, decodedFrames, len(t.detectFrames), maxPCM, streamInfo)
	}

	// Pick the ASC with the highest peak PCM.
	bestIdx := -1
	bestPCM := 0
	for i, r := range results {
		if r.err == nil && r.maxPCM > bestPCM {
			bestPCM = r.maxPCM
			bestIdx = i
		}
	}

	if bestIdx < 0 {
		return "", fmt.Errorf("no valid ASC candidate found")
	}

	fmt.Printf("[audio_detect] winner: ASC=%s maxPCM=%d\n", results[bestIdx].asc, bestPCM)
	return results[bestIdx].asc, nil
}

// transcodeFrame decodes one raw AAC-ELD frame and encodes to AAC-LC.
func (t *AudioTranscoder) transcodeFrame(aacELDFrame []byte) ([]byte, error) {
	// Decode AAC-ELD → PCM.
	nSamples := C.decode_frame(
		t.decoder,
		(*C.uchar)(unsafe.Pointer(&aacELDFrame[0])),
		C.int(len(aacELDFrame)),
		&t.decBuf[0],
		C.int(len(t.decBuf)),
	)
	if nSamples < 0 {
		return nil, fmt.Errorf("AAC-ELD decode failed: %d", nSamples)
	}

	decoded := int(nSamples)
	t.frameCount++

	// Log peak PCM from production decoder.
	if t.frameCount <= 5 || t.frameCount%50 == 0 {
		var maxPCM C.INT_PCM
		for i := 0; i < decoded; i++ {
			s := t.decBuf[i]
			if s < 0 {
				s = -s
			}
			if s > maxPCM {
				maxPCM = s
			}
		}
		fmt.Printf("[audio_transcoder] frame=%d decoded=%d maxPCM=%d sizeof(INT_PCM)=%d\n",
			t.frameCount, decoded, int(maxPCM), unsafe.Sizeof(t.decBuf[0]))
	}

	if t.pcmLen+decoded > len(t.pcmRing) {
		return nil, fmt.Errorf("PCM ring overflow: %d + %d > %d", t.pcmLen, decoded, len(t.pcmRing))
	}

	// Amplify decoded PCM. The camera's AAC-ELD decoder output is ~60 dB too
	// quiet (maxPCM ~7 out of 32767). Apply gain to bring to normal levels.
	// Clamp to int16 range to prevent clipping. Gain of 0 = passthrough.
	if t.gain != 0 {
		for i := 0; i < decoded; i++ {
			v := int32(t.decBuf[i]) * int32(t.gain)
			if v > 32767 {
				v = 32767
			} else if v < -32768 {
				v = -32768
			}
			t.decBuf[i] = C.INT_PCM(v)
		}
	}

	copy(t.pcmRing[t.pcmLen:], t.decBuf[:decoded])
	t.pcmLen += decoded

	// Not enough samples for an encoder frame yet.
	if t.pcmLen < t.encFrameSize {
		return nil, nil
	}

	// Encode one AAC-LC frame (1024 samples).
	n := C.encode_frame(
		t.encoder,
		&t.pcmRing[0],
		C.int(t.encFrameSize),
		(*C.uchar)(unsafe.Pointer(&t.encBuf[0])),
		C.int(len(t.encBuf)),
	)
	if n < 0 {
		return nil, fmt.Errorf("AAC-LC encode failed: %d", n)
	}

	// Shift remaining samples to front of ring.
	remaining := t.pcmLen - t.encFrameSize
	if remaining > 0 {
		copy(t.pcmRing[:remaining], t.pcmRing[t.encFrameSize:t.pcmLen])
	}
	t.pcmLen = remaining

	if n == 0 {
		return nil, nil
	}

	out := make([]byte, int(n))
	copy(out, t.encBuf[:int(n)])
	return out, nil
}

// TestDecodePeakPCM creates a temporary decoder to test-decode data and returns
// the peak absolute PCM sample value. Used for diagnostics to determine if
// AU headers should be stripped or not.
func (t *AudioTranscoder) TestDecodePeakPCM(data []byte) int {
	if len(data) == 0 {
		return -1
	}

	// Create a throwaway decoder with same ASC.
	dec := C.aacDecoder_Open(C.TT_MP4_RAW, 1)
	if dec == nil {
		return -2
	}
	defer C.aacDecoder_Close(dec)

	ascBytes, err := hex.DecodeString(t.eldASCHex)
	if err != nil {
		return -3
	}

	rc := C.config_raw(dec, (*C.uchar)(unsafe.Pointer(&ascBytes[0])), C.uint(len(ascBytes)))
	if rc != C.AAC_DEC_OK {
		return -4
	}

	tmpBuf := make([]C.INT_PCM, 8192)
	ns := C.decode_frame(dec, (*C.uchar)(unsafe.Pointer(&data[0])), C.int(len(data)), &tmpBuf[0], C.int(len(tmpBuf)))
	if ns <= 0 {
		return int(ns) // negative = decode error
	}

	var maxPCM C.INT_PCM
	for i := 0; i < int(ns); i++ {
		s := tmpBuf[i]
		if s < 0 {
			s = -s
		}
		if s > maxPCM {
			maxPCM = s
		}
	}
	return int(maxPCM)
}

// dumpFrame appends raw AAC and decoded PCM to files for offline analysis.
func (t *AudioTranscoder) dumpFrame(aacFrame []byte, decodedSamples int) {
	// Dump raw AAC frame (length-prefixed).
	if f, err := os.OpenFile("/tmp/audio_aac.bin", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		lenBuf := make([]byte, 4)
		binary.BigEndian.PutUint32(lenBuf, uint32(len(aacFrame)))
		f.Write(lenBuf)
		f.Write(aacFrame)
		f.Close()
	}

	// Dump decoded PCM (16-bit signed LE).
	if f, err := os.OpenFile("/tmp/audio_pcm.raw", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		pcmBytes := make([]byte, decodedSamples*2)
		for i := 0; i < decodedSamples; i++ {
			binary.LittleEndian.PutUint16(pcmBytes[i*2:], uint16(t.decBuf[i]))
		}
		f.Write(pcmBytes)
		f.Close()
	}
}

// selfTestELD runs a quick roundtrip test: encode a 1kHz sine wave as AAC-ELD,
// decode it back, and verify the PCM is non-trivial. This validates that FDK-AAC's
// ELD encoder+decoder actually work on this platform.
func selfTestELD(sampleRate int) {
	fmt.Printf("[audio_transcoder] self-test: encoding 1kHz sine as AAC-ELD at %dHz...\n", sampleRate)

	// Create ELD encoder.
	var enc C.HANDLE_AACENCODER
	if C.aacEncOpen(&enc, 0, 1) != C.AACENC_OK {
		fmt.Printf("[audio_transcoder] self-test: FAILED to open ELD encoder\n")
		return
	}
	defer C.aacEncClose(&enc)

	params := []struct {
		p C.AACENC_PARAM
		v int
	}{
		{C.AACENC_AOT, C.AOT_ER_AAC_ELD},
		{C.AACENC_SAMPLERATE, sampleRate},
		{C.AACENC_CHANNELMODE, C.MODE_1},
		{C.AACENC_BITRATE, 64000},
		{C.AACENC_TRANSMUX, C.TT_MP4_RAW},
	}
	for _, p := range params {
		if C.aacEncoder_SetParam(enc, p.p, C.UINT(p.v)) != C.AACENC_OK {
			fmt.Printf("[audio_transcoder] self-test: FAILED to set encoder param %d\n", p.p)
			return
		}
	}
	if C.aacEncEncode(enc, nil, nil, nil, nil) != C.AACENC_OK {
		fmt.Printf("[audio_transcoder] self-test: FAILED to init encoder\n")
		return
	}

	// Get encoder info (frame size + ASC).
	var info C.AACENC_InfoStruct
	if C.aacEncInfo(enc, &info) != C.AACENC_OK {
		fmt.Printf("[audio_transcoder] self-test: FAILED to get encoder info\n")
		return
	}
	frameSize := int(info.frameLength)
	fmt.Printf("[audio_transcoder] self-test: ELD encoder frameSize=%d\n", frameSize)

	// Extract ELD encoder's ASC.
	ascSize := int(info.confSize)
	eldASC := make([]byte, ascSize)
	for i := 0; i < ascSize; i++ {
		eldASC[i] = byte(info.confBuf[i])
	}
	fmt.Printf("[audio_transcoder] self-test: ELD encoder ASC=%X\n", eldASC)

	// Create decoder with the encoder's ASC.
	dec := C.aacDecoder_Open(C.TT_MP4_RAW, 1)
	if dec == nil {
		fmt.Printf("[audio_transcoder] self-test: FAILED to open decoder\n")
		return
	}
	defer C.aacDecoder_Close(dec)

	rc := C.config_raw(dec, (*C.uchar)(unsafe.Pointer(&eldASC[0])), C.uint(len(eldASC)))
	if rc != C.AAC_DEC_OK {
		fmt.Printf("[audio_transcoder] self-test: FAILED to configure decoder: %d\n", rc)
		return
	}

	// Generate 1kHz sine wave and encode+decode 10 frames.
	pcmIn := make([]C.INT_PCM, frameSize)
	encOut := make([]byte, 2048)
	decOut := make([]C.INT_PCM, 8192)

	for frame := 0; frame < 10; frame++ {
		// Fill PCM with 1kHz sine at ~50% amplitude.
		for i := 0; i < frameSize; i++ {
			sample := int(frame)*frameSize + i
			// sin(2*pi*1000*t/sampleRate) * 16000
			// Use integer approximation to avoid importing math.
			phase := (sample * 1000 * 4) / sampleRate // quarter-periods
			switch phase % 4 {
			case 0:
				pcmIn[i] = 0
			case 1:
				pcmIn[i] = 16000
			case 2:
				pcmIn[i] = 0
			case 3:
				pcmIn[i] = -16000
			}
		}

		// Encode.
		n := C.encode_frame(enc, &pcmIn[0], C.int(frameSize),
			(*C.uchar)(unsafe.Pointer(&encOut[0])), C.int(len(encOut)))
		if n <= 0 {
			fmt.Printf("[audio_transcoder] self-test: encode failed at frame %d: %d\n", frame, n)
			continue
		}

		// Decode.
		ns := C.decode_frame(dec, (*C.uchar)(unsafe.Pointer(&encOut[0])), C.int(n),
			&decOut[0], C.int(len(decOut)))
		if ns <= 0 {
			fmt.Printf("[audio_transcoder] self-test: decode failed at frame %d: %d\n", frame, ns)
			continue
		}

		// Measure peak PCM.
		var maxPCM C.INT_PCM
		for i := 0; i < int(ns); i++ {
			s := decOut[i]
			if s < 0 {
				s = -s
			}
			if s > maxPCM {
				maxPCM = s
			}
		}
		fmt.Printf("[audio_transcoder] self-test: frame=%d encoded=%d decoded=%d maxPCM=%d\n",
			frame, int(n), int(ns), int(maxPCM))
	}
}

// AudioSpecificConfig returns the AAC-LC AudioSpecificConfig from the encoder,
// suitable for use in SDP config= parameter.
func (t *AudioTranscoder) AudioSpecificConfig() []byte {
	return t.outASC
}

// AudioSpecificConfigHex returns the ASC as a hex string for SDP.
func (t *AudioTranscoder) AudioSpecificConfigHex() string {
	return strings.ToUpper(hex.EncodeToString(t.outASC))
}

// TalkbackTranscoder converts RTSP backchannel G.711 audio into HomeKit's
// AAC-ELD RTP payload format.
type TalkbackTranscoder struct {
	encoder         C.HANDLE_AACENCODER
	sampleRate      int
	gain            int
	encFrameSize    int
	rtpFrameSamples int
	pcmRing         []C.INT_PCM
	pcmLen          int
	encBuf          []byte
	lastPCM         int16
	haveLastPCM     bool
}

type TalkbackFrame struct {
	Payload []byte
	Samples uint32
	Padded  bool
}

const talkbackPeakLimit = 22000

func NewTalkbackTranscoder(sampleRate int, gain int) (*TalkbackTranscoder, error) {
	if sampleRate == 0 {
		sampleRate = 16000
	}
	if gain < 1 {
		gain = 1
	}
	if sampleRate%8000 != 0 {
		return nil, fmt.Errorf("talkback sample rate %d is not an integer multiple of 8000", sampleRate)
	}

	var encoder C.HANDLE_AACENCODER
	if e := C.aacEncOpen(&encoder, 0, 1); e != C.AACENC_OK {
		return nil, fmt.Errorf("aacEncOpen talkback failed: %d", e)
	}

	targetFrameSamples := 512

	t := &TalkbackTranscoder{
		encoder:         encoder,
		sampleRate:      sampleRate,
		gain:            gain,
		rtpFrameSamples: targetFrameSamples,
		encBuf:          make([]byte, 2048),
	}

	params := []struct {
		param C.AACENC_PARAM
		value int
	}{
		{C.AACENC_AOT, C.AOT_ER_AAC_ELD},
		{C.AACENC_SAMPLERATE, sampleRate},
		{C.AACENC_CHANNELMODE, C.MODE_1},
		{C.AACENC_BITRATE, 24000},
		// The G100's camera-to-controller AAC-ELD stream decodes to 512-sample
		// frames, and the return path sounds time-stretched when forced to
		// short 480-sample frames. Use the native ELD frame cadence here too.
		{C.AACENC_GRANULE_LENGTH, targetFrameSamples},
		{C.AACENC_TRANSMUX, C.TT_MP4_RAW},
	}
	for _, p := range params {
		if e := C.aacEncoder_SetParam(t.encoder, p.param, C.UINT(p.value)); e != C.AACENC_OK {
			t.Close()
			return nil, fmt.Errorf("aacEncoder_SetParam talkback(%d) failed: %d", p.param, e)
		}
	}

	if e := C.aacEncEncode(t.encoder, nil, nil, nil, nil); e != C.AACENC_OK {
		t.Close()
		return nil, fmt.Errorf("aacEncEncode talkback init failed: %d", e)
	}

	var info C.AACENC_InfoStruct
	if e := C.aacEncInfo(t.encoder, &info); e != C.AACENC_OK {
		t.Close()
		return nil, fmt.Errorf("aacEncInfo talkback failed: %d", e)
	}

	t.encFrameSize = int(info.frameLength)
	t.rtpFrameSamples = t.encFrameSize
	t.pcmRing = make([]C.INT_PCM, t.encFrameSize*4)
	silence := make([]C.INT_PCM, t.encFrameSize)
	if n := C.encode_frame(t.encoder, &silence[0], C.int(t.encFrameSize),
		(*C.uchar)(unsafe.Pointer(&t.encBuf[0])), C.int(len(t.encBuf))); n < 0 {
		t.Close()
		return nil, fmt.Errorf("AAC-ELD talkback encoder priming failed: %d", n)
	}
	return t, nil
}

func (t *TalkbackTranscoder) FrameDuration() time.Duration {
	if t == nil || t.sampleRate <= 0 || t.rtpFrameSamples <= 0 {
		return 30 * time.Millisecond
	}
	return time.Duration(t.rtpFrameSamples) * time.Second / time.Duration(t.sampleRate)
}

func (t *TalkbackTranscoder) FrameDurationForSamples(samples uint32) time.Duration {
	if t == nil || t.sampleRate <= 0 {
		return 30 * time.Millisecond
	}
	if samples == 0 {
		samples = uint32(t.rtpFrameSamples)
	}
	return time.Duration(samples) * time.Second / time.Duration(t.sampleRate)
}

func (t *TalkbackTranscoder) TranscodeG711(payload []byte, mulaw bool) ([]TalkbackFrame, int, int, error) {
	if len(payload) == 0 {
		return nil, 0, 0, nil
	}

	inPeak := 0
	decoded := make([]int16, len(payload))
	for i, b := range payload {
		pcm := decodeG711Sample(b, mulaw)
		decoded[i] = pcm
		if v := absInt16(pcm); v > inPeak {
			inPeak = v
		}
	}

	gainNumer, gainDenom := t.gain, 1
	if inPeak > 0 && int64(inPeak)*int64(t.gain) > talkbackPeakLimit {
		gainNumer = talkbackPeakLimit
		gainDenom = inPeak
	}

	upsample := t.sampleRate / 8000
	outPeak := 0
	for _, pcm := range decoded {
		pcm = scaleInt16(pcm, gainNumer, gainDenom)
		if v := absInt16(pcm); v > outPeak {
			outPeak = v
		}
		if !t.haveLastPCM {
			t.lastPCM = pcm
			t.haveLastPCM = true
		}
		for i := 1; i <= upsample; i++ {
			t.appendTalkbackPCM(interpolateInt16(t.lastPCM, pcm, i, upsample))
		}
		t.lastPCM = pcm
	}

	var out []TalkbackFrame
	for t.pcmLen >= t.encFrameSize {
		n := C.encode_frame(t.encoder, &t.pcmRing[0], C.int(t.encFrameSize),
			(*C.uchar)(unsafe.Pointer(&t.encBuf[0])), C.int(len(t.encBuf)))
		if n < 0 {
			return nil, inPeak, outPeak, fmt.Errorf("AAC-ELD talkback encode failed: %d", n)
		}
		if n > 0 {
			payload := make([]byte, 4+int(n))
			binary.BigEndian.PutUint16(payload[0:2], 16)
			binary.BigEndian.PutUint16(payload[2:4], uint16(n)<<3)
			copy(payload[4:], t.encBuf[:int(n)])
			out = append(out, TalkbackFrame{Payload: payload})
		}

		copy(t.pcmRing, t.pcmRing[t.encFrameSize:t.pcmLen])
		t.pcmLen -= t.encFrameSize
	}

	for i := range out {
		out[i].Samples = uint32(t.encFrameSize)
	}
	return out, inPeak, outPeak, nil
}

func (t *TalkbackTranscoder) appendTalkbackPCM(pcm int16) {
	if t.pcmLen >= len(t.pcmRing) {
		copy(t.pcmRing, t.pcmRing[t.encFrameSize:t.pcmLen])
		t.pcmLen -= t.encFrameSize
	}
	t.pcmRing[t.pcmLen] = C.INT_PCM(pcm)
	t.pcmLen++
}

func interpolateInt16(from int16, to int16, step int, steps int) int16 {
	if steps <= 1 {
		return to
	}
	v := int(from) + (int(to)-int(from))*step/steps
	return int16(v)
}

func scaleInt16(v int16, numer int, denom int) int16 {
	if denom <= 0 {
		denom = 1
	}
	if numer == denom {
		return v
	}
	scaled := int64(v) * int64(numer) / int64(denom)
	return clipInt16(scaled)
}

func clipInt16(v int64) int16 {
	if v > 32767 {
		return 32767
	}
	if v < -32768 {
		return -32768
	}
	return int16(v)
}

func absInt16(v int16) int {
	n := int(v)
	if n < 0 {
		return -n
	}
	return n
}

func decodeG711Sample(v byte, mulaw bool) int16 {
	if mulaw {
		u := ^v
		sign := u & 0x80
		exponent := (u >> 4) & 0x07
		mantissa := u & 0x0F
		sample := int(((uint16(mantissa) << 3) + 0x84) << exponent)
		sample -= 0x84
		if sign != 0 {
			return int16(-sample)
		}
		return int16(sample)
	}

	a := v ^ 0x55
	sign := a & 0x80
	exponent := (a >> 4) & 0x07
	mantissa := a & 0x0F
	sample := int(mantissa) << 4
	if exponent == 0 {
		sample += 8
	} else {
		sample += 0x108
		sample <<= exponent - 1
	}
	if sign == 0 {
		return int16(-sample)
	}
	return int16(sample)
}

// Close releases all FDK-AAC resources.
func (t *AudioTranscoder) Close() {
	if t.decoder != nil {
		C.aacDecoder_Close(t.decoder)
		t.decoder = nil
	}
	if t.encoder != nil {
		C.aacEncClose(&t.encoder)
		t.encoder = nil
	}
}

func (t *TalkbackTranscoder) Close() {
	if t.encoder != nil {
		C.aacEncClose(&t.encoder)
		t.encoder = nil
	}
}
