package stream

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

// SRTPProxy receives SRTP packets from a HomeKit camera, decrypts them,
// and forwards them as plain RTP. Unlike pion/srtp's SessionSRTP, this
// implementation manually handles SRTP/SRTCP demuxing (like go2rtc) so
// we can see and respond to RTCP packets from the camera.
type SRTPProxy struct {
	logger *slog.Logger

	videoConn *net.UDPConn
	audioConn *net.UDPConn

	// Manual SRTP decryption contexts (camera's keys, for decrypting incoming).
	videoDecryptCtx *srtp.Context
	audioDecryptCtx *srtp.Context

	// SRTCP encryption context (controller's keys, for encrypting outgoing RTCP).
	srtcpEncryptCtx *srtp.Context

	// Audio return SRTP context (controller's keys, for encrypting outgoing audio).
	audioReturnCtx *srtp.Context
	audioSRTCPCtx  *srtp.Context

	// Camera's remote addresses.
	cameraAddr      *net.UDPAddr
	cameraAudioAddr *net.UDPAddr

	// SSRCs.
	controllerSSRC  uint32 // our own video SSRC
	cameraVideoSSRC uint32 // camera's video SSRC (discovered from packets)
	audioReturnSSRC uint32 // our own audio SSRC

	// Audio return state.
	audioReturnMu           sync.Mutex
	audioReturnSeq          uint16
	audioReturnTS           uint32
	audioReturnInputBaseTS  uint32
	audioReturnOutputBaseTS uint32
	audioReturnHaveInputTS  bool
	lastTalkbackPacket      time.Time
	talkbackPacketsSent     uint64
	audioReturnPacketsSent  uint32
	audioReturnOctetsSent   uint32

	// Track highest received video seq for ReceptionReport.
	rtcpMu               sync.Mutex
	videoSeqInitialized  bool
	videoBaseSeq         uint16
	videoMaxSeq          uint16
	videoSeqCycles       uint32
	videoPacketsReceived uint32
	highestVideoSeq      uint32

	// Callbacks for forwarding decrypted packets.
	onVideoRTP   func(*rtp.Packet)
	onAudioRTP   func(*rtp.Packet)
	onVideoStats func(VideoStats)

	stopCh chan struct{}
	wg     sync.WaitGroup
}

type VideoStats struct {
	TotalPackets       uint64
	TotalDrops         uint64
	TotalDroppedFrames uint64
	DeltaPackets       uint64
	DeltaDrops         uint64
	DeltaDroppedFrames uint64
	DecryptErrors      uint64
}

// SRTPConfig holds the SRTP keys from the SetupEndpoints exchange.
type SRTPConfig struct {
	VideoKey  []byte
	VideoSalt []byte
	AudioKey  []byte
	AudioSalt []byte
	VideoSSRC uint32
	AudioSSRC uint32
	// Camera's address for sending RTCP keepalives and audio return.
	CameraAddr      *net.UDPAddr
	CameraAudioAddr *net.UDPAddr
	// Controller's own SRTP keys and SSRCs (what we sent to the camera).
	// Used for encrypting SRTCP and audio return sent back to the camera.
	ControllerVideoKey  []byte
	ControllerVideoSalt []byte
	ControllerVideoSSRC uint32
	ControllerAudioKey  []byte
	ControllerAudioSalt []byte
	ControllerAudioSSRC uint32
}

// NewSRTPProxy creates a new SRTP-to-RTP proxy.
func NewSRTPProxy(logger *slog.Logger) *SRTPProxy {
	return &SRTPProxy{
		logger: logger,
		stopCh: make(chan struct{}),
	}
}

// SetCallbacks sets the packet forwarding callbacks.
func (p *SRTPProxy) SetCallbacks(onVideo, onAudio func(*rtp.Packet)) {
	p.onVideoRTP = onVideo
	p.onAudioRTP = onAudio
}

func (p *SRTPProxy) SetVideoStatsCallback(callback func(VideoStats)) {
	p.onVideoStats = callback
}

// OpenPorts opens UDP listeners on the specified ports (or random ports if 0).
// Returns the actual video and audio ports. Call this before SetupEndpoints.
func (p *SRTPProxy) OpenPorts(videoPort, audioPort int) (int, int, error) {
	var err error

	p.stopCh = make(chan struct{})

	p.videoConn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: videoPort})
	if err != nil {
		return 0, 0, fmt.Errorf("listen video UDP: %w", err)
	}

	p.audioConn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: audioPort})
	if err != nil {
		p.videoConn.Close()
		p.videoConn = nil
		return 0, 0, fmt.Errorf("listen audio UDP: %w", err)
	}

	// Increase socket read buffers to survive RTP bursts (e.g. large IDR frames).
	// If the kernel's rmem_max is too low, the actual buffer may be smaller.
	if err := p.videoConn.SetReadBuffer(2 * 1024 * 1024); err != nil {
		p.logger.Warn("failed to set video UDP read buffer (check net.core.rmem_max)", "error", err)
	}
	if err := p.audioConn.SetReadBuffer(512 * 1024); err != nil {
		p.logger.Warn("failed to set audio UDP read buffer (check net.core.rmem_max)", "error", err)
	}

	actualVideoPort := p.videoConn.LocalAddr().(*net.UDPAddr).Port
	actualAudioPort := p.audioConn.LocalAddr().(*net.UDPAddr).Port

	return actualVideoPort, actualAudioPort, nil
}

// Start sets up SRTP contexts on already-opened ports and begins decrypting.
func (p *SRTPProxy) Start(cfg SRTPConfig) error {
	if p.videoConn == nil || p.audioConn == nil {
		return fmt.Errorf("ports not opened; call OpenPorts first")
	}

	var err error

	// Create SRTP decryption contexts using the camera's keys.
	p.videoDecryptCtx, err = srtp.CreateContext(
		cfg.VideoKey, cfg.VideoSalt,
		srtp.ProtectionProfileAes128CmHmacSha1_80,
	)
	if err != nil {
		p.Close()
		return fmt.Errorf("create video decrypt context: %w", err)
	}

	p.audioDecryptCtx, err = srtp.CreateContext(
		cfg.AudioKey, cfg.AudioSalt,
		srtp.ProtectionProfileAes128CmHmacSha1_80,
	)
	if err != nil {
		p.Close()
		return fmt.Errorf("create audio decrypt context: %w", err)
	}

	// Create SRTCP encryption context using the CONTROLLER's video keys.
	if len(cfg.ControllerVideoKey) > 0 {
		p.srtcpEncryptCtx, err = srtp.CreateContext(
			cfg.ControllerVideoKey, cfg.ControllerVideoSalt,
			srtp.ProtectionProfileAes128CmHmacSha1_80,
		)
		if err != nil {
			p.logger.Warn("failed to create SRTCP encrypt context", "error", err)
		}
	}

	// Create audio return SRTP/SRTCP contexts using the CONTROLLER's audio keys.
	if len(cfg.ControllerAudioKey) > 0 {
		p.audioReturnCtx, err = srtp.CreateContext(
			cfg.ControllerAudioKey, cfg.ControllerAudioSalt,
			srtp.ProtectionProfileAes128CmHmacSha1_80,
		)
		if err != nil {
			p.logger.Warn("failed to create audio return context", "error", err)
		}
		p.audioSRTCPCtx, err = srtp.CreateContext(
			cfg.ControllerAudioKey, cfg.ControllerAudioSalt,
			srtp.ProtectionProfileAes128CmHmacSha1_80,
		)
		if err != nil {
			p.logger.Warn("failed to create audio SRTCP context", "error", err)
		}
	}

	// Store addresses and SSRCs.
	p.cameraAddr = cfg.CameraAddr
	p.cameraAudioAddr = cfg.CameraAudioAddr
	p.controllerSSRC = cfg.ControllerVideoSSRC
	p.cameraVideoSSRC = cfg.VideoSSRC
	p.audioReturnSSRC = cfg.ControllerAudioSSRC
	p.audioReturnSeq = 0
	p.audioReturnTS = 0
	p.audioReturnInputBaseTS = 0
	p.audioReturnOutputBaseTS = 0
	p.audioReturnHaveInputTS = false
	p.lastTalkbackPacket = time.Time{}
	p.talkbackPacketsSent = 0
	p.audioReturnPacketsSent = 0
	p.audioReturnOctetsSent = 0

	// Start goroutines.
	p.wg.Add(5)
	go p.readVideoLoop()
	go p.readAudioLoop()
	go p.rtcpKeepaliveLoop()
	go p.audioReturnLoop()
	go p.audioRTCPReportLoop()

	p.logger.Info("SRTP proxy started",
		"video_port", p.videoConn.LocalAddr().(*net.UDPAddr).Port,
		"audio_port", p.audioConn.LocalAddr().(*net.UDPAddr).Port,
		"camera_video_ssrc", cfg.VideoSSRC,
		"camera_audio_ssrc", cfg.AudioSSRC,
		"controller_video_ssrc", cfg.ControllerVideoSSRC,
		"controller_audio_ssrc", cfg.ControllerAudioSSRC)

	return nil
}

// readVideoLoop reads raw UDP packets from the video port, demuxes RTP vs RTCP,
// and handles each appropriately. Packets are buffered per-frame and only
// forwarded as complete frames to prevent partial-frame decoder artifacts.
func (p *SRTPProxy) readVideoLoop() {
	defer p.wg.Done()
	buf := make([]byte, 2048)
	var videoCount, rtcpCount, dropCount, decryptErrors, droppedFrames uint64
	var lastSeq uint16
	var lastFrameTS uint32
	var frameGapped bool       // true if current frame has a sequence gap
	var frameBuf []*rtp.Packet // buffered packets for current frame
	lastLogTime := time.Now()
	var lastStatsPackets, lastStatsDrops, lastStatsDroppedFrames uint64

	for {
		n, _, err := p.videoConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-p.stopCh:
			default:
				p.logger.Error("video UDP read error", "error", err)
			}
			p.logger.Info("video read loop exiting", "video_packets", videoCount, "rtcp_packets", rtcpCount)
			return
		}

		if n < 2 {
			continue
		}

		// RFC 5761 demuxing: payload type byte distinguishes RTP from RTCP.
		// RTCP packet types are 200-207, RTP payload types are typically < 128.
		pt := buf[1] & 0x7F // strip marker bit for RTP
		if buf[1] >= 200 && buf[1] <= 207 {
			// SRTCP packet from camera.
			rtcpCount++
			p.handleVideoRTCP(buf[:n])
			continue
		}

		// SRTP packet - decrypt manually.
		header := &rtp.Header{}
		decrypted, err := p.videoDecryptCtx.DecryptRTP(nil, buf[:n], header)
		if err != nil {
			decryptErrors++
			if decryptErrors <= 3 || decryptErrors%100 == 0 {
				p.logger.Warn("video SRTP decrypt error",
					"error", err, "size", n, "pt_byte", pt,
					"total_errors", decryptErrors)
			}
			continue
		}

		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(decrypted); err != nil {
			p.logger.Warn("video RTP unmarshal error", "error", err)
			continue
		}

		videoCount++

		// Track camera's actual SSRC and highest seq.
		if videoCount == 1 {
			lastFrameTS = pkt.Header.Timestamp
			p.logger.Info("first video RTP packet received",
				"pt", pkt.Header.PayloadType,
				"ssrc", pkt.Header.SSRC,
				"seq", pkt.Header.SequenceNumber,
				"size", n)
		} else {
			// Detect RTP sequence gaps (dropped UDP packets). Small backwards
			// jumps are usually UDP reordering, not 65k dropped packets.
			expected := lastSeq + 1
			if pkt.Header.SequenceNumber != expected {
				forwardGap := int(pkt.Header.SequenceNumber) - int(expected)
				if forwardGap < 0 {
					forwardGap += 65536
				}
				lateGap := int(expected) - int(pkt.Header.SequenceNumber)
				if lateGap < 0 {
					lateGap += 65536
				}

				if lateGap > 0 && lateGap < forwardGap {
					p.logger.Debug("late video RTP packet ignored",
						"expected", expected,
						"got", pkt.Header.SequenceNumber,
						"late_by", lateGap)
					continue
				}

				dropCount += uint64(forwardGap)
				frameGapped = true
				p.logger.Warn("video RTP sequence gap",
					"expected", expected,
					"got", pkt.Header.SequenceNumber,
					"gap", forwardGap,
					"total_drops", dropCount)
			}
		}
		lastSeq = pkt.Header.SequenceNumber
		p.updateVideoReceptionStats(pkt.Header.SequenceNumber, pkt.Header.SSRC)

		// Frame boundary: new RTP timestamp means new frame.
		// Flush previous frame buffer (forward if complete, drop if gapped).
		if pkt.Header.Timestamp != lastFrameTS && len(frameBuf) > 0 {
			if !frameGapped && p.onVideoRTP != nil {
				for _, fp := range frameBuf {
					p.onVideoRTP(fp)
				}
			} else if frameGapped {
				droppedFrames++
			}
			frameBuf = frameBuf[:0]

			// If the gap crossed into this new frame (first packet isn't
			// a frame start), carry the gapped flag forward.
			if frameGapped {
				frameGapped = len(pkt.Payload) > 0 && !isFrameStart(pkt.Payload)
			}
		}
		lastFrameTS = pkt.Header.Timestamp

		// Safety cap: if buffer grows too large (e.g. missing marker packets),
		// drop and reset.
		if len(frameBuf) >= 300 {
			p.logger.Warn("frame buffer overflow, dropping", "buffered", len(frameBuf))
			frameBuf = frameBuf[:0]
			frameGapped = true
		}

		// Buffer the packet.
		frameBuf = append(frameBuf, pkt)

		// End of frame (marker bit): flush the complete frame.
		if pkt.Header.Marker {
			if !frameGapped && p.onVideoRTP != nil {
				for _, fp := range frameBuf {
					p.onVideoRTP(fp)
				}
			} else if frameGapped {
				droppedFrames++
			}
			frameBuf = frameBuf[:0]
			frameGapped = false
		}

		// Log every second to track camera packet rate.
		if time.Since(lastLogTime) >= time.Second {
			stats := VideoStats{
				TotalPackets:       videoCount,
				TotalDrops:         dropCount,
				TotalDroppedFrames: droppedFrames,
				DeltaPackets:       videoCount - lastStatsPackets,
				DeltaDrops:         dropCount - lastStatsDrops,
				DeltaDroppedFrames: droppedFrames - lastStatsDroppedFrames,
				DecryptErrors:      decryptErrors,
			}
			p.logger.Info("video RTP stats",
				"total_packets", videoCount,
				"rtcp_packets", rtcpCount,
				"drops", dropCount,
				"dropped_frames", droppedFrames,
				"decrypt_errors", decryptErrors,
				"seq", pkt.Header.SequenceNumber,
				"ts", pkt.Header.Timestamp)
			if p.onVideoStats != nil {
				p.onVideoStats(stats)
			}
			lastStatsPackets = videoCount
			lastStatsDrops = dropCount
			lastStatsDroppedFrames = droppedFrames
			lastLogTime = time.Now()
		}
	}
}

// isFrameStart checks if an RTP payload begins a new H.264 access unit.
// Returns true for: single NALUs (types 1-23), STAP-A (type 24),
// or FU-A (type 28) with the start bit set.
func isFrameStart(payload []byte) bool {
	if len(payload) < 1 {
		return false
	}
	naluType := payload[0] & 0x1F
	switch {
	case naluType >= 1 && naluType <= 24:
		// Single NALU or STAP-A — always a frame/AU start.
		return true
	case naluType == 28 && len(payload) > 1:
		// FU-A: only a frame start if the start bit is set.
		return payload[1]&0x80 != 0
	}
	return false
}

// handleVideoRTCP processes an SRTCP packet received from the camera on the
// video port. Per go2rtc's implementation, when we receive a SenderReport,
// we immediately respond with a ReceiverReport.
func (p *SRTPProxy) handleVideoRTCP(data []byte) {
	// Decrypt SRTCP using camera's keys.
	header := rtcp.Header{}
	decrypted, err := p.videoDecryptCtx.DecryptRTCP(nil, data, &header)
	if err != nil {
		// Not all RTCP-looking packets will decrypt successfully.
		return
	}

	p.logger.Debug("received RTCP from camera",
		"type", header.Type,
		"length", len(decrypted))

	// Respond to SenderReport with ReceiverReport (like go2rtc).
	if header.Type == rtcp.TypeSenderReport {
		p.sendRTCPKeepalive()
	}
}

func (p *SRTPProxy) updateVideoReceptionStats(seq uint16, ssrc uint32) {
	p.rtcpMu.Lock()
	defer p.rtcpMu.Unlock()

	if ssrc != 0 {
		p.cameraVideoSSRC = ssrc
	}
	if !p.videoSeqInitialized {
		p.videoSeqInitialized = true
		p.videoBaseSeq = seq
		p.videoMaxSeq = seq
		p.videoPacketsReceived = 1
		p.highestVideoSeq = uint32(seq)
		return
	}

	// RTP sequence numbers are 16-bit. RTCP Receiver Reports require the
	// extended highest sequence number, including wrap cycles.
	if seq < p.videoMaxSeq && p.videoMaxSeq-seq > 30000 {
		p.videoSeqCycles += 1 << 16
		p.videoMaxSeq = seq
	} else if seq > p.videoMaxSeq {
		p.videoMaxSeq = seq
	}
	p.videoPacketsReceived++
	p.highestVideoSeq = p.videoSeqCycles + uint32(p.videoMaxSeq)
}

func (p *SRTPProxy) rtcpReceptionSnapshot() (ssrc uint32, highestSeq uint32, totalLost uint32, ok bool) {
	p.rtcpMu.Lock()
	defer p.rtcpMu.Unlock()

	if !p.videoSeqInitialized || p.cameraVideoSSRC == 0 {
		return 0, 0, 0, false
	}

	expected := int64(p.highestVideoSeq-uint32(p.videoBaseSeq)) + 1
	lost := expected - int64(p.videoPacketsReceived)
	if lost < 0 {
		lost = 0
	}
	if lost > 0x7fffff {
		lost = 0x7fffff
	}
	return p.cameraVideoSSRC, p.highestVideoSeq, uint32(lost), true
}

// readAudioLoop reads raw UDP packets from the audio port and decrypts them.
func (p *SRTPProxy) readAudioLoop() {
	defer p.wg.Done()
	buf := make([]byte, 2048)
	var count uint64

	for {
		n, _, err := p.audioConn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-p.stopCh:
			default:
				p.logger.Error("audio UDP read error", "error", err)
			}
			p.logger.Info("audio read loop exiting", "packets", count)
			return
		}

		if n < 2 {
			continue
		}

		// Skip RTCP packets on audio port.
		if buf[1] >= 200 && buf[1] <= 207 {
			continue
		}

		// Decrypt SRTP.
		header := &rtp.Header{}
		decrypted, err := p.audioDecryptCtx.DecryptRTP(nil, buf[:n], header)
		if err != nil {
			continue
		}

		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(decrypted); err != nil {
			continue
		}

		count++
		if count == 1 {
			p.logger.Info("first audio RTP packet received",
				"pt", pkt.Header.PayloadType,
				"ssrc", pkt.Header.SSRC,
				"seq", pkt.Header.SequenceNumber,
				"size", n)
		}

		if p.onAudioRTP != nil {
			p.onAudioRTP(pkt)
		}
	}
}

// rtcpKeepaliveLoop sends periodic SRTCP Receiver Reports to the camera.
func (p *SRTPProxy) rtcpKeepaliveLoop() {
	defer p.wg.Done()

	if p.cameraAddr == nil || p.videoConn == nil {
		p.logger.Warn("RTCP keepalive disabled: no camera address")
		return
	}

	p.logger.Info("RTCP keepalive started",
		"cameraAddr", p.cameraAddr,
		"controllerSSRC", p.controllerSSRC,
		"cameraVideoSSRC", p.cameraVideoSSRC)

	// Send first RTCP immediately.
	p.sendRTCPKeepalive()

	// Use 0.5s interval matching the video RTCPInterval we negotiate.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.sendRTCPKeepalive()
		}
	}
}

// sendRTCPKeepalive sends an SRTCP ReceiverReport to the camera.
func (p *SRTPProxy) sendRTCPKeepalive() {
	if p.srtcpEncryptCtx == nil {
		return
	}

	cameraSSRC, highestSeq, totalLost, ok := p.rtcpReceptionSnapshot()
	if !ok {
		p.logger.Debug("skipping RTCP RR until first video packet")
		return
	}

	rr := &rtcp.ReceiverReport{
		SSRC: p.controllerSSRC,
		Reports: []rtcp.ReceptionReport{{
			SSRC:               cameraSSRC,
			LastSequenceNumber: highestSeq,
			TotalLost:          totalLost,
		}},
	}
	rrBytes, err := rr.Marshal()
	if err != nil {
		p.logger.Warn("marshal RTCP RR", "error", err)
		return
	}

	encrypted, err := p.srtcpEncryptCtx.EncryptRTCP(nil, rrBytes, nil)
	if err != nil {
		p.logger.Warn("encrypt SRTCP RR", "error", err)
		return
	}

	if _, err := p.videoConn.WriteToUDP(encrypted, p.cameraAddr); err != nil {
		p.logger.Warn("send SRTCP to camera", "error", err)
	}

	p.logger.Debug("sent RTCP keepalive",
		"controllerSSRC", p.controllerSSRC,
		"cameraSSRC", cameraSSRC,
		"highestSeq", highestSeq,
		"totalLost", totalLost,
		"addr", p.cameraAddr)
}

// audioReturnLoop sends low-rate silence SRTP packets to the camera's audio
// port only as a keepalive fallback. Real talk-back packets sent through
// SendAudioReturnPacket suppress the fallback while talk-back is active.
func (p *SRTPProxy) audioReturnLoop() {
	defer p.wg.Done()

	if p.audioReturnCtx == nil || p.cameraAudioAddr == nil {
		p.logger.Warn("audio return disabled")
		return
	}

	p.logger.Info("audio return started",
		"cameraAudioAddr", p.cameraAudioAddr,
		"ssrc", p.audioReturnSSRC)

	// Some HomeKit cameras stop the stream when the return path is completely
	// idle. Keep the path warm at low rate, then back off while real talk-back
	// audio is flowing.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	silencePayload := []byte{0x00, 0x00, 0x00, 0x00}

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			if p.talkbackActive() {
				continue
			}
			p.sendAudioReturn(silencePayload, true, p.reserveSilenceTimestamp())
		}
	}
}

func (p *SRTPProxy) audioRTCPReportLoop() {
	defer p.wg.Done()

	if p.audioSRTCPCtx == nil || p.cameraAudioAddr == nil || p.audioConn == nil {
		return
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			if p.talkbackActive() {
				p.sendAudioSenderReport()
			}
		}
	}
}

func (p *SRTPProxy) reserveSilenceTimestamp() uint32 {
	p.audioReturnMu.Lock()
	defer p.audioReturnMu.Unlock()

	timestamp := p.audioReturnTS
	p.audioReturnTS += 320
	return timestamp
}

func (p *SRTPProxy) talkbackActive() bool {
	p.audioReturnMu.Lock()
	defer p.audioReturnMu.Unlock()
	return !p.lastTalkbackPacket.IsZero() && time.Since(p.lastTalkbackPacket) < 2*time.Second
}

func (p *SRTPProxy) audioReturnStatsSnapshot() (rtpTime uint32, packets uint32, octets uint32) {
	p.audioReturnMu.Lock()
	defer p.audioReturnMu.Unlock()
	return p.audioReturnTS, p.audioReturnPacketsSent, p.audioReturnOctetsSent
}

func (p *SRTPProxy) sendAudioSenderReport() {
	rtpTime, packets, octets := p.audioReturnStatsSnapshot()
	now := time.Now()
	ntp := toNTPTime(now)

	sr := &rtcp.SenderReport{
		SSRC:        p.audioReturnSSRC,
		NTPTime:     ntp,
		RTPTime:     rtpTime,
		PacketCount: packets,
		OctetCount:  octets,
	}

	raw, err := sr.Marshal()
	if err != nil {
		p.logger.Warn("marshal audio RTCP SR", "error", err)
		return
	}

	encrypted, err := p.audioSRTCPCtx.EncryptRTCP(nil, raw, nil)
	if err != nil {
		p.logger.Warn("encrypt audio SRTCP SR", "error", err)
		return
	}

	if _, err := p.audioConn.WriteToUDP(encrypted, p.cameraAudioAddr); err != nil {
		p.logger.Warn("send audio SRTCP SR to camera", "error", err)
		return
	}

	p.logger.Debug("sent audio RTCP sender report",
		"ssrc", p.audioReturnSSRC,
		"rtp_ts", rtpTime,
		"packets", packets,
		"octets", octets)
}

func toNTPTime(t time.Time) uint64 {
	const ntpEpochOffset = 2208988800
	secs := uint64(t.Unix() + ntpEpochOffset)
	frac := uint64(uint32((uint64(t.Nanosecond()) << 32) / 1e9))
	return secs<<32 | frac
}

// SendAudioReturnPacket encrypts and sends an RTP audio packet to the HomeKit
// camera return path. The packet payload must already be encoded in the codec
// negotiated with the camera (usually AAC-ELD or Opus).
func (p *SRTPProxy) SendAudioReturnPacket(pkt *rtp.Packet) {
	if pkt == nil || len(pkt.Payload) == 0 {
		return
	}
	if p.audioReturnCtx == nil || p.cameraAudioAddr == nil || p.audioConn == nil {
		return
	}

	p.audioReturnMu.Lock()
	now := time.Now()
	if !p.audioReturnHaveInputTS || (!p.lastTalkbackPacket.IsZero() && now.Sub(p.lastTalkbackPacket) > 2*time.Second) {
		p.audioReturnInputBaseTS = pkt.Header.Timestamp
		p.audioReturnOutputBaseTS = p.audioReturnTS
		p.audioReturnHaveInputTS = true
	}
	timestamp := p.audioReturnOutputBaseTS + (pkt.Header.Timestamp - p.audioReturnInputBaseTS)
	p.audioReturnTS = timestamp
	p.lastTalkbackPacket = now
	p.talkbackPacketsSent++
	sent := p.talkbackPacketsSent
	p.audioReturnMu.Unlock()

	if sent == 1 || sent%50 == 0 {
		p.logger.Info("HomeKit talkback SRTP sent",
			"packets", sent,
			"payload", len(pkt.Payload),
			"rtp_ts", timestamp,
			"cameraAudioAddr", p.cameraAudioAddr)
	}

	p.sendAudioReturn(pkt.Payload, pkt.Header.Marker, timestamp)
}

// sendAudioReturn sends a single SRTP audio packet to the camera.
func (p *SRTPProxy) sendAudioReturn(payload []byte, marker bool, timestamp uint32) {
	p.audioReturnMu.Lock()
	defer p.audioReturnMu.Unlock()

	seq := p.audioReturnSeq
	p.audioReturnSeq++
	p.audioReturnPacketsSent++
	p.audioReturnOctetsSent += uint32(len(payload))

	pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    110,
			SequenceNumber: seq,
			Timestamp:      timestamp,
			SSRC:           p.audioReturnSSRC,
		},
		Payload: payload,
	}

	raw, err := pkt.Marshal()
	if err != nil {
		return
	}

	encrypted, err := p.audioReturnCtx.EncryptRTP(nil, raw, nil)
	if err != nil {
		return
	}

	if _, err := p.audioConn.WriteToUDP(encrypted, p.cameraAudioAddr); err != nil {
		p.logger.Warn("send audio return to camera", "error", err)
	}
}

// Close stops the proxy and releases resources.
func (p *SRTPProxy) Close() {
	select {
	case <-p.stopCh:
		return // Already closed.
	default:
		close(p.stopCh)
	}

	if p.videoConn != nil {
		p.videoConn.Close()
	}
	if p.audioConn != nil {
		p.audioConn.Close()
	}

	p.wg.Wait()
	p.logger.Info("SRTP proxy stopped")
}
