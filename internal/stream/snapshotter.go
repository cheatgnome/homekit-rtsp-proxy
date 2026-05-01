package stream

/*
#cgo LDFLAGS: -lavcodec -lavutil -lswscale
#include <libavcodec/avcodec.h>
#include <libavutil/imgutils.h>
#include <stdlib.h>
#include <string.h>

// snap_ctx holds the H.264 decoder + MJPEG encoder state.
typedef struct {
    AVCodecContext* dec;     // H.264 decoder
    AVCodecContext* enc;     // MJPEG encoder (lazily inited once dimensions known)
    AVFrame*        yuv;     // Reused decode-output frame
    AVPacket*       in_pkt;  // Reused decode-input packet
    AVPacket*       out_pkt; // Reused encode-output packet
} snap_ctx;

static snap_ctx* snap_new(void) {
    snap_ctx* s = (snap_ctx*)calloc(1, sizeof(snap_ctx));
    if (!s) return NULL;
    const AVCodec* h264 = avcodec_find_decoder(AV_CODEC_ID_H264);
    if (!h264) { free(s); return NULL; }
    s->dec = avcodec_alloc_context3(h264);
    if (!s->dec) { free(s); return NULL; }
    if (avcodec_open2(s->dec, h264, NULL) < 0) {
        avcodec_free_context(&s->dec);
        free(s);
        return NULL;
    }
    s->yuv     = av_frame_alloc();
    s->in_pkt  = av_packet_alloc();
    s->out_pkt = av_packet_alloc();
    if (!s->yuv || !s->in_pkt || !s->out_pkt) {
        if (s->yuv)     av_frame_free(&s->yuv);
        if (s->in_pkt)  av_packet_free(&s->in_pkt);
        if (s->out_pkt) av_packet_free(&s->out_pkt);
        avcodec_free_context(&s->dec);
        free(s);
        return NULL;
    }
    return s;
}

static void snap_free(snap_ctx* s) {
    if (!s) return;
    if (s->dec)     avcodec_free_context(&s->dec);
    if (s->enc)     avcodec_free_context(&s->enc);
    if (s->yuv)     av_frame_free(&s->yuv);
    if (s->in_pkt)  av_packet_free(&s->in_pkt);
    if (s->out_pkt) av_packet_free(&s->out_pkt);
    free(s);
}

// snap_decode pushes the given Annex-B bytestream (SPS+PPS+IDR is sufficient)
// into the H.264 decoder and pulls one frame. *width/*height are filled on
// success. Returns 0 on success, negative on error.
static int snap_decode(snap_ctx* s,
                       unsigned char* data, int size,
                       int* width, int* height) {
    s->in_pkt->data = data;
    s->in_pkt->size = size;
    int ret = avcodec_send_packet(s->dec, s->in_pkt);
    if (ret < 0) return ret;
    ret = avcodec_receive_frame(s->dec, s->yuv);
    if (ret < 0) return ret;
    *width  = s->yuv->width;
    *height = s->yuv->height;
    return 0;
}

// snap_init_encoder lazily builds the MJPEG encoder once we know the
// stream's dimensions. quality is 1 (best) .. 31 (worst); 3 is a sane
// default for snapshot use.
static int snap_init_encoder(snap_ctx* s, int width, int height, int quality) {
    if (s->enc) return 0;
    const AVCodec* mjpeg = avcodec_find_encoder(AV_CODEC_ID_MJPEG);
    if (!mjpeg) return -1;
    s->enc = avcodec_alloc_context3(mjpeg);
    if (!s->enc) return -1;
    s->enc->pix_fmt        = AV_PIX_FMT_YUVJ420P;
    s->enc->width          = width;
    s->enc->height         = height;
    s->enc->time_base.num  = 1;
    s->enc->time_base.den  = 25;
    s->enc->flags         |= AV_CODEC_FLAG_QSCALE;
    s->enc->global_quality = quality * FF_QP2LAMBDA;
    if (avcodec_open2(s->enc, mjpeg, NULL) < 0) {
        avcodec_free_context(&s->enc);
        return -1;
    }
    return 0;
}

// snap_encode_jpeg encodes the cached YUV frame as a JPEG. On success
// *out points to a malloc'd buffer of *outSize bytes (caller must free).
static int snap_encode_jpeg(snap_ctx* s, unsigned char** out, int* outSize) {
    // The H.264 decoder produces YUV420P (limited range). MJPEG encoder
    // wants YUVJ420P (full range). They share layout, so a relabel is
    // sufficient — no pixel shuffling required.
    s->yuv->format = AV_PIX_FMT_YUVJ420P;

    int ret = avcodec_send_frame(s->enc, s->yuv);
    if (ret < 0) return ret;
    ret = avcodec_receive_packet(s->enc, s->out_pkt);
    if (ret < 0) return ret;

    *out = (unsigned char*)malloc(s->out_pkt->size);
    if (!*out) {
        av_packet_unref(s->out_pkt);
        return -1;
    }
    memcpy(*out, s->out_pkt->data, s->out_pkt->size);
    *outSize = s->out_pkt->size;
    av_packet_unref(s->out_pkt);
    return 0;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"unsafe"

	"github.com/pion/rtp"
)

// Snapshotter decodes cached H.264 IDR frames into JPEG snapshots and
// caches the latest result for HTTP serving.
type Snapshotter struct {
	logger *slog.Logger

	mu     sync.RWMutex
	ctx    *C.snap_ctx
	jpeg   []byte
	width  int
	height int
}

// NewSnapshotter creates a Snapshotter with an initialized H.264 decoder.
// The MJPEG encoder is created lazily on first frame.
func NewSnapshotter(logger *slog.Logger) (*Snapshotter, error) {
	ctx := C.snap_new()
	if ctx == nil {
		return nil, errors.New("snapshotter: failed to allocate H.264 decoder")
	}
	return &Snapshotter{logger: logger, ctx: ctx}, nil
}

// Close releases all underlying libavcodec resources.
func (s *Snapshotter) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx != nil {
		C.snap_free(s.ctx)
		s.ctx = nil
	}
}

// Update decodes the given Annex-B bytestream (must contain at least
// SPS+PPS+IDR) and re-encodes the result as JPEG. The cached JPEG is
// replaced atomically.
func (s *Snapshotter) Update(annexB []byte) error {
	if len(annexB) == 0 {
		return errors.New("snapshotter: empty bitstream")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil {
		return errors.New("snapshotter: closed")
	}

	var w, h C.int
	if rc := C.snap_decode(s.ctx, (*C.uchar)(unsafe.Pointer(&annexB[0])),
		C.int(len(annexB)), &w, &h); rc < 0 {
		return fmt.Errorf("snapshotter: H.264 decode failed (avcodec %d)", int(rc))
	}

	if rc := C.snap_init_encoder(s.ctx, w, h, 3); rc < 0 {
		return errors.New("snapshotter: MJPEG encoder init failed")
	}

	var out *C.uchar
	var outSize C.int
	if rc := C.snap_encode_jpeg(s.ctx, &out, &outSize); rc < 0 {
		return fmt.Errorf("snapshotter: MJPEG encode failed (avcodec %d)", int(rc))
	}
	defer C.free(unsafe.Pointer(out))

	s.jpeg = C.GoBytes(unsafe.Pointer(out), outSize)
	s.width = int(w)
	s.height = int(h)
	return nil
}

// LatestJPEG returns the most recently encoded snapshot, or nil if no
// frame has been decoded yet.
func (s *Snapshotter) LatestJPEG() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.jpeg
}

// BuildIDRAnnexB reassembles the cached IDR-frame RTP packets into an
// Annex-B H.264 bytestream prefixed with the given SPS and PPS, suitable
// for feeding to a libavcodec H.264 decoder.
//
// The cache typically holds: one STAP-A packet (SPS+PPS, sometimes with
// an SEI), followed by FU-A fragments that together carry the IDR slice.
// We re-emit cached SPS/PPS first (the camera only sends them at session
// start, so this guarantees the decoder sees them) and then append every
// reassembled NALU from the cache.
func BuildIDRAnnexB(idrCache []*rtp.Packet, sps, pps []byte) []byte {
	startCode := []byte{0, 0, 0, 1}
	out := make([]byte, 0, 64*1024)
	if len(sps) > 0 {
		out = append(out, startCode...)
		out = append(out, sps...)
	}
	if len(pps) > 0 {
		out = append(out, startCode...)
		out = append(out, pps...)
	}

	var fuPayload []byte
	var fuNALByte byte

	for _, pkt := range idrCache {
		if len(pkt.Payload) == 0 {
			continue
		}
		header := pkt.Payload[0]
		naluType := header & 0x1F

		switch {
		case naluType >= 1 && naluType <= 23:
			// Single NAL unit; emit as-is.
			out = append(out, startCode...)
			out = append(out, pkt.Payload...)

		case naluType == 24:
			// STAP-A: walk aggregated NALUs.
			payload := pkt.Payload[1:]
			for len(payload) >= 2 {
				size := int(payload[0])<<8 | int(payload[1])
				payload = payload[2:]
				if size <= 0 || size > len(payload) {
					break
				}
				out = append(out, startCode...)
				out = append(out, payload[:size]...)
				payload = payload[size:]
			}

		case naluType == 28:
			// FU-A: reassemble fragments.
			if len(pkt.Payload) < 2 {
				continue
			}
			fuHeader := pkt.Payload[1]
			startBit := fuHeader & 0x80
			endBit := fuHeader & 0x40
			origType := fuHeader & 0x1F
			if startBit != 0 {
				// Reconstruct original NALU header: NRI bits from FU
				// indicator + original type from FU header.
				fuNALByte = (header & 0xE0) | origType
				fuPayload = fuPayload[:0]
				fuPayload = append(fuPayload, fuNALByte)
			}
			fuPayload = append(fuPayload, pkt.Payload[2:]...)
			if endBit != 0 && len(fuPayload) > 0 {
				out = append(out, startCode...)
				out = append(out, fuPayload...)
				fuPayload = fuPayload[:0]
			}
		}
	}
	return out
}
