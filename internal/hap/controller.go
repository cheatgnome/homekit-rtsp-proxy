package hap

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hkontrol/hkontroller"
)

// HAP service and characteristic type UUIDs (short form).
const (
	// CameraRTPStreamManagement service
	ServiceCameraRTPStreamMgmt = "110"
	CharSetupEndpoints         = "118"
	CharSelectedRTPConfig      = "117"
	CharStreamingStatus        = "120"
	CharSupportedVideoConfig   = "114"
	CharSupportedAudioConfig   = "115"

	// MotionSensor service
	ServiceMotionSensor        = "85"
	CharMotionDetected         = "22"
)

// cameraCharIDs holds the discovered characteristic IIDs for a camera.
type cameraCharIDs struct {
	streams           []cameraStreamCharIDs
	motionDetectedAID int
	motionDetectedIID int
}

type cameraStreamCharIDs struct {
	index                int
	aid                  int
	setupEndpointsIID    int
	selectedConfigIID    int
	streamingStatusIID   int
	supportedVideoCfgIID int
	supportedAudioCfgIID int
	videoConfig          SupportedVideoConfig
}

type streamStartOption struct {
	ids   cameraStreamCharIDs
	video VideoSelection
}

type VideoOption struct {
	Channel int
	Video   VideoSelection
}

// Controller manages HAP connections to HomeKit cameras.
type Controller struct {
	store      *PairingStore
	logger     *slog.Logger
	bindAddr   string
	storePath  string // path to hkontroller file store

	mu              sync.Mutex
	devices         map[string]*hkontroller.Device
	verified        map[string]*VerifiedConn // our custom verified connections
	charIDs         map[string]*cameraCharIDs
	activeStreams   map[string]*cameraStreamCharIDs
	streamStartMu   map[string]*sync.Mutex
	motionCallbacks map[string]func(bool) // stored for re-subscribe after reconnect
	motionCtx       context.Context       // context for motion subscriptions
	controller      *hkontroller.Controller
	cancelFunc      context.CancelFunc

	// recoveredCallback is fired after a successful auto-recovery (e.g. after
	// a camera reboot). main.go uses it to restart any active stream so the
	// new HAP session and fresh SRTP keys take effect.
	recoveredCallback func(deviceName string)
}

// NewController creates a new HAP controller.
func NewController(store *PairingStore, bindAddr string, logger *slog.Logger) *Controller {
	return &Controller{
		store:     store,
		logger:    logger,
		bindAddr:  bindAddr,
		storePath:       "./.hkontroller",
		devices:         make(map[string]*hkontroller.Device),
		verified:        make(map[string]*VerifiedConn),
		charIDs:         make(map[string]*cameraCharIDs),
		activeStreams:   make(map[string]*cameraStreamCharIDs),
		streamStartMu:   make(map[string]*sync.Mutex),
		motionCallbacks: make(map[string]func(bool)),
	}
}

// Start begins mDNS discovery and connects to known devices.
func (c *Controller) Start(ctx context.Context) error {
	ctrl, err := hkontroller.NewController(
		hkontroller.NewFsStore(c.storePath),
		"homekit-rtsp-proxy",
	)
	if err != nil {
		return fmt.Errorf("create hkontroller: %w", err)
	}
	c.controller = ctrl

	// Load previously paired devices.
	if err := ctrl.LoadPairings(); err != nil {
		c.logger.Warn("failed to load pairings", "error", err)
	}

	// Start mDNS discovery.
	dctx, cancel := context.WithCancel(ctx)
	c.cancelFunc = cancel

	discoverCh, lostCh := ctrl.StartDiscoveryWithContext(dctx)
	go c.discoveryLoop(dctx, discoverCh, lostCh)

	return nil
}

// SetRecoveredCallback registers a function to be called after a successful
// auto-recovery (post-reconnect of a previously-lost device). main.go uses
// this to restart any active stream so the fresh HAP session/SRTP keys
// drive new packets without disturbing connected RTSP clients.
func (c *Controller) SetRecoveredCallback(cb func(deviceName string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recoveredCallback = cb
}

// discoveryLoop merges the discover/lost channels so we can track which
// devices are currently lost and trigger an auto-recovery the moment a
// paired device reappears (typical pattern for a camera reboot).
func (c *Controller) discoveryLoop(ctx context.Context, discoverCh, lostCh <-chan *hkontroller.Device) {
	lost := make(map[string]bool)
	for {
		select {
		case <-ctx.Done():
			return
		case device, ok := <-discoverCh:
			if !ok {
				discoverCh = nil
				if lostCh == nil {
					return
				}
				continue
			}
			wasLost := lost[device.Name]
			delete(lost, device.Name)
			c.mu.Lock()
			c.devices[device.Name] = device
			c.mu.Unlock()
			c.logger.Info("device discovered",
				"name", device.Name,
				"paired", device.IsPaired(),
				"wasLost", wasLost)
			if wasLost && device.IsPaired() {
				go c.recoverDevice(ctx, device.Name)
			}
		case device, ok := <-lostCh:
			if !ok {
				lostCh = nil
				if discoverCh == nil {
					return
				}
				continue
			}
			c.logger.Warn("device lost", "name", device.Name)
			lost[device.Name] = true
		}
	}
}

// recoverDevice re-establishes the HAP session for a device that was
// rediscovered after being lost (most commonly after a camera reboot).
// It retries with backoff to handle the camera still booting, then fires
// the registered recovered callback so any in-flight stream can be
// restarted with new SRTP keys.
func (c *Controller) recoverDevice(ctx context.Context, deviceName string) {
	// Long enough to span a typical camera boot (~30–60s) without being
	// excessive. After exhaustion we wait for the next mDNS lost/rediscover
	// cycle to trigger another attempt.
	backoff := []time.Duration{0, 5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}
	for i, wait := range backoff {
		if wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		c.logger.Info("auto-recovery attempt", "name", deviceName, "attempt", i+1)
		if err := c.reconnect(deviceName); err != nil {
			c.logger.Warn("auto-recovery attempt failed",
				"name", deviceName, "attempt", i+1, "error", err)
			continue
		}
		c.logger.Info("auto-recovery succeeded", "name", deviceName)
		c.mu.Lock()
		cb := c.recoveredCallback
		c.mu.Unlock()
		if cb != nil {
			cb(deviceName)
		}
		return
	}
	c.logger.Error("auto-recovery exhausted; waiting for next mDNS event",
		"name", deviceName)
}

// PairCamera pairs with a camera using its setup code, then performs pair-verify
// using our custom implementation (which correctly omits the Method TLV).
func (c *Controller) PairCamera(ctx context.Context, deviceName, setupCode string) error {
	c.mu.Lock()
	device, ok := c.devices[deviceName]
	if !ok {
		// mDNS names may contain backslash-escaped spaces; try matching.
		for name, dev := range c.devices {
			cleanName := strings.ReplaceAll(name, "\\", "")
			if cleanName == deviceName {
				device = dev
				ok = true
				break
			}
		}
	}
	c.mu.Unlock()

	if !ok {
		// Try looking up via controller.
		device = c.controller.GetDevice(deviceName)
		if device == nil {
			// Try all discovered devices.
			for _, d := range c.controller.GetAllDevices() {
				cleanName := strings.ReplaceAll(d.Name, "\\", "")
				if cleanName == deviceName || d.Name == deviceName {
					device = d
					break
				}
			}
		}
		if device == nil {
			return fmt.Errorf("device %q not found via mDNS", deviceName)
		}
	}

	// Step 1: Pair-setup via hkontroller (this part works fine).
	if !device.IsPaired() {
		c.logger.Info("performing pair-setup", "name", deviceName)
		if err := device.PairSetup(setupCode); err != nil {
			return fmt.Errorf("pair setup: %w", err)
		}
		c.logger.Info("pair-setup complete", "name", deviceName)
		// Close the pair-setup connection.
		device.Close()
	}

	// Step 2: Get the keys we need for pair-verify.
	// Read controller keypair from hkontroller store.
	controllerID, controllerLTSK, controllerLTPK, err := c.readControllerKeys()
	if err != nil {
		return fmt.Errorf("read controller keys: %w", err)
	}

	// Read accessory pairing info.
	pairingInfo := device.GetPairingInfo()
	if len(pairingInfo.PublicKey) == 0 {
		return fmt.Errorf("no accessory public key for %q (not paired?)", deviceName)
	}

	c.logger.Info("accessory pairing info",
		"name", deviceName,
		"id", pairingInfo.Id,
		"pubkey_len", len(pairingInfo.PublicKey))

	// Step 3: Get the device's IP:port from mDNS.
	entry := device.GetDnssdEntry()
	if len(entry.IPs) == 0 {
		return fmt.Errorf("no IPs known for %q", deviceName)
	}

	// Prefer IPv4.
	var deviceAddr string
	for _, ip := range entry.IPs {
		if ip.To4() != nil {
			deviceAddr = fmt.Sprintf("%s:%d", ip.String(), entry.Port)
			break
		}
	}
	if deviceAddr == "" {
		deviceAddr = fmt.Sprintf("[%s]:%d", entry.IPs[0].String(), entry.Port)
	}

	// Step 4: Perform pair-verify using our custom implementation.
	c.logger.Info("performing pair-verify", "name", deviceName, "addr", deviceAddr)

	var vc *VerifiedConn
	var verifyErr error
	for attempt := 1; attempt <= 3; attempt++ {
		vc, verifyErr = DoPairVerify(deviceAddr, controllerID, controllerLTSK, controllerLTPK, pairingInfo.PublicKey)
		if verifyErr == nil {
			break
		}
		c.logger.Warn("pair-verify attempt failed", "name", deviceName, "attempt", attempt, "error", verifyErr)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	if verifyErr != nil {
		return fmt.Errorf("pair verify: %w", verifyErr)
	}

	// Step 5: Discover accessory database to find characteristic IIDs.
	c.logger.Info("fetching accessory database", "name", deviceName)
	accessories, err := vc.Client.GetAccessories()
	if err != nil {
		vc.Client.Close()
		return fmt.Errorf("get accessories: %w", err)
	}

	ids, err := findCameraCharIDs(accessories)
	if err != nil {
		vc.Client.Close()
		return fmt.Errorf("find camera characteristics: %w", err)
	}
	if err := loadSupportedVideoConfigs(vc.Client, ids); err != nil {
		c.logger.Warn("failed to read supported video configs", "name", deviceName, "error", err)
	}

	c.logger.Info("camera characteristics found",
		"name", deviceName,
		"stream_channels", len(ids.streams),
		"streams", describeStreams(ids.streams),
		"hasMotion", ids.motionDetectedIID != 0)

	// Read SupportedAudioStreamConfiguration to discover supported codecs.
	if ids.streams[0].supportedAudioCfgIID != 0 {
		audioResp, err := vc.Client.GetCharacteristics(
			fmt.Sprintf("%d.%d", ids.streams[0].aid, ids.streams[0].supportedAudioCfgIID),
		)
		if err == nil && len(audioResp.Characteristics) > 0 {
			c.logger.Info("supported audio config (raw base64)",
				"name", deviceName, "value", audioResp.Characteristics[0].Value)
		}
	}

	c.mu.Lock()
	c.verified[deviceName] = vc
	c.charIDs[deviceName] = ids
	c.mu.Unlock()

	c.logger.Info("camera paired and verified", "name", deviceName)
	return nil
}

// readControllerKeys reads the controller's Ed25519 keypair from the hkontroller store.
func (c *Controller) readControllerKeys() (controllerID string, ltsk ed25519.PrivateKey, ltpk ed25519.PublicKey, err error) {
	data, err := os.ReadFile(c.storePath + "/keypair")
	if err != nil {
		return "", nil, nil, fmt.Errorf("read keypair: %w", err)
	}

	var kp struct {
		Public  []byte `json:"Public"`
		Private []byte `json:"Private"`
	}
	if err := json.Unmarshal(data, &kp); err != nil {
		return "", nil, nil, fmt.Errorf("parse keypair: %w", err)
	}

	// hkontroller uses the controller name (passed to NewController) as the controllerId
	// during pair-setup M5. We must use the same ID for pair-verify.
	controllerID = "homekit-rtsp-proxy"

	return controllerID, ed25519.PrivateKey(kp.Private), ed25519.PublicKey(kp.Public), nil
}

// reconnect re-establishes the HAP pair-verify session for a camera whose
// TCP connection has dropped (broken pipe, EOF, etc.). It discovers the
// camera's current address via mDNS, performs pair-verify, and re-fetches
// the accessory database to update characteristic IIDs.
func (c *Controller) reconnect(deviceName string) error {
	c.logger.Info("reconnecting to camera", "name", deviceName)

	controllerID, controllerLTSK, controllerLTPK, err := c.readControllerKeys()
	if err != nil {
		return fmt.Errorf("read controller keys: %w", err)
	}

	c.mu.Lock()
	device, ok := c.devices[deviceName]
	c.mu.Unlock()
	if !ok {
		device = c.controller.GetDevice(deviceName)
		if device == nil {
			return fmt.Errorf("device %q not found via mDNS", deviceName)
		}
	}

	pairingInfo := device.GetPairingInfo()
	if len(pairingInfo.PublicKey) == 0 {
		return fmt.Errorf("no accessory public key for %q", deviceName)
	}

	entry := device.GetDnssdEntry()
	if len(entry.IPs) == 0 {
		return fmt.Errorf("no IPs known for %q", deviceName)
	}

	var deviceAddr string
	for _, ip := range entry.IPs {
		if ip.To4() != nil {
			deviceAddr = fmt.Sprintf("%s:%d", ip.String(), entry.Port)
			break
		}
	}
	if deviceAddr == "" {
		deviceAddr = fmt.Sprintf("[%s]:%d", entry.IPs[0].String(), entry.Port)
	}

	c.logger.Info("performing pair-verify (reconnect)", "name", deviceName, "addr", deviceAddr)

	var vc *VerifiedConn
	var verifyErr error
	for attempt := 1; attempt <= 3; attempt++ {
		vc, verifyErr = DoPairVerify(deviceAddr, controllerID, controllerLTSK, controllerLTPK, pairingInfo.PublicKey)
		if verifyErr == nil {
			break
		}
		c.logger.Warn("pair-verify attempt failed (reconnect)", "name", deviceName, "attempt", attempt, "error", verifyErr)
		time.Sleep(time.Duration(attempt) * time.Second)
	}
	if verifyErr != nil {
		return fmt.Errorf("pair verify: %w", verifyErr)
	}

	c.logger.Info("fetching accessory database (reconnect)", "name", deviceName)
	accessories, err := vc.Client.GetAccessories()
	if err != nil {
		vc.Client.Close()
		return fmt.Errorf("get accessories: %w", err)
	}

	ids, err := findCameraCharIDs(accessories)
	if err != nil {
		vc.Client.Close()
		return fmt.Errorf("find camera characteristics: %w", err)
	}
	if err := loadSupportedVideoConfigs(vc.Client, ids); err != nil {
		c.logger.Warn("failed to read supported video configs after reconnect", "name", deviceName, "error", err)
	}

	c.mu.Lock()
	c.verified[deviceName] = vc
	c.charIDs[deviceName] = ids
	motionCb := c.motionCallbacks[deviceName]
	motionCtx := c.motionCtx
	c.mu.Unlock()

	// Re-subscribe to motion events on the new connection.
	if motionCb != nil && motionCtx != nil {
		if err := c.doSubscribeMotion(motionCtx, deviceName, motionCb); err != nil {
			c.logger.Warn("failed to re-subscribe motion after reconnect", "name", deviceName, "error", err)
		} else {
			c.logger.Info("motion sensor re-subscribed after reconnect", "name", deviceName)
		}
	}

	c.logger.Info("camera reconnected", "name", deviceName)
	return nil
}

// isConnError returns true if the error indicates a broken TCP connection
// (broken pipe, connection reset, EOF) that warrants a reconnect.
func isConnError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset") ||
		strings.Contains(s, "EOF") ||
		strings.Contains(s, "use of closed network connection")
}

func isRecoverableStreamSetupError(err error) bool {
	if err == nil {
		return false
	}
	if isConnError(err) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "read SetupEndpoints response") ||
		strings.Contains(s, "SetupEndpoints response not ready") ||
		strings.Contains(s, "HTTP 204")
}

func (c *Controller) streamStartLock(deviceName string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	lock := c.streamStartMu[deviceName]
	if lock == nil {
		lock = &sync.Mutex{}
		c.streamStartMu[deviceName] = lock
	}
	return lock
}

// StartStream initiates a camera stream and returns the stream response.
// If the HAP connection or stream setup state looks stale, it automatically
// reconnects only that camera and retries once.
func (c *Controller) StartStream(ctx context.Context, deviceName string, localIP net.IP, videoPort, audioPort uint16, videoConfig VideoSelection, audioConfig AudioSelection, preferredChannel int) (*StreamResponse, error) {
	lock := c.streamStartLock(deviceName)
	lock.Lock()
	defer lock.Unlock()

	resp, err := c.doStartStream(ctx, deviceName, localIP, videoPort, audioPort, videoConfig, audioConfig, preferredChannel)
	if err != nil && isRecoverableStreamSetupError(err) {
		c.logger.Warn("stream start failed with recoverable HAP error, reconnecting camera", "name", deviceName, "error", err)
		if reconErr := c.reconnect(deviceName); reconErr != nil {
			return nil, fmt.Errorf("reconnect failed: %w (original: %v)", reconErr, err)
		}
		return c.doStartStream(ctx, deviceName, localIP, videoPort, audioPort, videoConfig, audioConfig, preferredChannel)
	}
	return resp, err
}

func (c *Controller) ResolveVideoSelection(deviceName string, videoConfig VideoSelection, preferredChannel int) (VideoSelection, int, error) {
	options, err := c.ResolveVideoOptions(deviceName, videoConfig, preferredChannel)
	if err != nil {
		return videoConfig, 0, err
	}
	if len(options) == 0 {
		return videoConfig, 0, fmt.Errorf("device %q has no usable stream channels", deviceName)
	}
	return options[0].Video, options[0].Channel, nil
}

func (c *Controller) ResolveVideoOptions(deviceName string, videoConfig VideoSelection, preferredChannel int) ([]VideoOption, error) {
	c.mu.Lock()
	ids := c.charIDs[deviceName]
	c.mu.Unlock()

	if ids == nil {
		return nil, fmt.Errorf("device %q characteristics not discovered", deviceName)
	}
	if len(ids.streams) == 0 {
		return nil, fmt.Errorf("device %q has no camera stream-management services", deviceName)
	}

	options, err := selectStreamOptions(ids.streams, videoConfig, preferredChannel)
	if err != nil {
		return nil, err
	}
	resolved := make([]VideoOption, 0, len(options))
	for _, option := range options {
		resolved = append(resolved, VideoOption{
			Channel: option.ids.index,
			Video:   option.video,
		})
	}
	return resolved, nil
}

func (c *Controller) doStartStream(ctx context.Context, deviceName string, localIP net.IP, videoPort, audioPort uint16, videoConfig VideoSelection, audioConfig AudioSelection, preferredChannel int) (*StreamResponse, error) {
	c.mu.Lock()
	vc := c.verified[deviceName]
	ids := c.charIDs[deviceName]
	c.mu.Unlock()

	if vc == nil {
		return nil, fmt.Errorf("device %q not verified", deviceName)
	}
	if ids == nil {
		return nil, fmt.Errorf("device %q characteristics not discovered", deviceName)
	}
	if len(ids.streams) == 0 {
		return nil, fmt.Errorf("device %q has no camera stream-management services", deviceName)
	}

	client := vc.Client
	options, err := selectStreamOptions(ids.streams, videoConfig, preferredChannel)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt, option := range options {
		streamResp, err := c.tryStartStream(ctx, client, deviceName, option.ids, localIP, videoPort, audioPort, option.video, audioConfig)
		if err == nil {
			c.mu.Lock()
			active := option.ids
			c.activeStreams[deviceName] = &active
			c.mu.Unlock()
			return streamResp, nil
		}
		lastErr = err
		if len(options) > 1 {
			c.logger.Warn("stream channel start failed, trying next channel",
				"name", deviceName, "channel", option.ids.index, "attempt", attempt+1, "error", err)
		}
	}

	return nil, lastErr
}

func (c *Controller) tryStartStream(ctx context.Context, client *HAPClient, deviceName string, ids cameraStreamCharIDs, localIP net.IP, videoPort, audioPort uint16, videoConfig VideoSelection, audioConfig AudioSelection) (*StreamResponse, error) {
	// Step 1: Generate stream endpoints with random SRTP keys.
	ep, err := GenerateStreamEndpoints(localIP, videoPort, audioPort)
	if err != nil {
		return nil, fmt.Errorf("generate endpoints: %w", err)
	}

	// Step 2: Write SetupEndpoints characteristic (TLV8 encoded, base64).
	setupTLV := EncodeSetupEndpoints(ep)
	setupB64 := base64.StdEncoding.EncodeToString(setupTLV)

	c.logger.Debug("writing SetupEndpoints", "name", deviceName,
		"aid", ids.aid, "iid", ids.setupEndpointsIID,
		"channel", ids.index, "localIP", localIP, "videoPort", videoPort, "audioPort", audioPort)

	err = client.PutCharacteristics([]Characteristic{
		{AID: ids.aid, IID: ids.setupEndpointsIID, Value: setupB64},
	})
	if err != nil {
		return nil, fmt.Errorf("write SetupEndpoints: %w", err)
	}

	// Step 3: Read back SetupEndpoints to get camera's response. Some cameras
	// briefly return an empty/non-string value immediately after accepting the
	// write, so wait until the TLV response is actually ready.
	respB64, err := c.readSetupEndpointsResponse(ctx, client, deviceName, ids)
	if err != nil {
		return nil, fmt.Errorf("read SetupEndpoints response: %w", err)
	}

	respTLV, err := base64.StdEncoding.DecodeString(respB64)
	if err != nil {
		return nil, fmt.Errorf("decode response base64: %w", err)
	}

	streamResp, err := DecodeStreamResponse(respTLV)
	if err != nil {
		return nil, fmt.Errorf("decode stream response: %w", err)
	}

	if streamResp.Status != 0 {
		return nil, fmt.Errorf("camera rejected stream setup (status=%d)", streamResp.Status)
	}

	c.logger.Info("stream setup accepted",
		"name", deviceName,
		"channel", ids.index,
		"remoteIP", streamResp.RemoteIP,
		"videoPort", streamResp.RemoteVideoPort,
		"audioPort", streamResp.RemoteAudioPort,
		"videoSSRC", streamResp.VideoSSRC,
		"audioSSRC", streamResp.AudioSSRC)

	// Step 4: Set SSRCs and write SelectedRTPStreamConfiguration with start command.
	videoConfig.SSRC = randomUint32()
	audioConfig.SSRC = randomUint32()

	selectedTLV := EncodeSelectedStreamConfig(ep.SessionID, SessionCommandStart, videoConfig, audioConfig)
	selectedB64 := base64.StdEncoding.EncodeToString(selectedTLV)

	c.logger.Debug("SelectedRTPStreamConfiguration TLV8",
		"hex", fmt.Sprintf("%x", selectedTLV),
		"base64", selectedB64)

	err = client.PutCharacteristics([]Characteristic{
		{AID: ids.aid, IID: ids.selectedConfigIID, Value: selectedB64},
	})
	if err != nil {
		return nil, fmt.Errorf("write SelectedRTPStreamConfiguration: %w", err)
	}

	c.logger.Info("stream started", "name", deviceName, "channel", ids.index,
		"width", videoConfig.Width, "height", videoConfig.Height, "fps", videoConfig.FPS)

	// Use the camera's SRTP keys for decryption. If the camera didn't return
	// keys (some cameras use the controller's keys for both directions),
	// fall back to the keys we sent.
	if len(streamResp.VideoSRTPKey) == 0 {
		c.logger.Debug("camera returned no video SRTP key, using ours")
		streamResp.VideoSRTPKey = ep.VideoSRTPKey[:]
		streamResp.VideoSRTPSalt = ep.VideoSRTPSalt[:]
	} else {
		c.logger.Debug("using camera's video SRTP key",
			"key_len", len(streamResp.VideoSRTPKey),
			"salt_len", len(streamResp.VideoSRTPSalt))
	}
	if len(streamResp.AudioSRTPKey) == 0 {
		c.logger.Debug("camera returned no audio SRTP key, using ours")
		streamResp.AudioSRTPKey = ep.AudioSRTPKey[:]
		streamResp.AudioSRTPSalt = ep.AudioSRTPSalt[:]
	} else {
		c.logger.Debug("using camera's audio SRTP key",
			"key_len", len(streamResp.AudioSRTPKey),
			"salt_len", len(streamResp.AudioSRTPSalt))
	}

	// Store controller's own keys and SSRCs for SRTCP encryption and audio return.
	// Outbound RTCP/RTP from controller must be encrypted with controller's keys.
	streamResp.ControllerVideoKey = ep.VideoSRTPKey[:]
	streamResp.ControllerVideoSalt = ep.VideoSRTPSalt[:]
	streamResp.ControllerVideoSSRC = videoConfig.SSRC
	streamResp.ControllerAudioKey = ep.AudioSRTPKey[:]
	streamResp.ControllerAudioSalt = ep.AudioSRTPSalt[:]
	streamResp.ControllerAudioSSRC = audioConfig.SSRC

	return streamResp, nil
}

func (c *Controller) readSetupEndpointsResponse(ctx context.Context, client *HAPClient, deviceName string, ids cameraStreamCharIDs) (string, error) {
	charID := fmt.Sprintf("%d.%d", ids.aid, ids.setupEndpointsIID)
	var lastErr error
	for attempt := 1; attempt <= 10; attempt++ {
		resp, err := client.GetCharacteristics(charID)
		if err == nil {
			if len(resp.Characteristics) == 0 {
				lastErr = fmt.Errorf("empty SetupEndpoints response")
			} else if respB64, ok := resp.Characteristics[0].Value.(string); ok && respB64 != "" {
				return respB64, nil
			} else {
				lastErr = fmt.Errorf("SetupEndpoints response not ready: %T", resp.Characteristics[0].Value)
			}
		} else {
			lastErr = err
			if !strings.Contains(err.Error(), "HTTP 204") {
				return "", err
			}
		}
		c.logger.Debug("SetupEndpoints response not ready, retrying",
			"name", deviceName,
			"channel", ids.index,
			"attempt", attempt,
			"error", lastErr)

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
		}
	}
	return "", lastErr
}

// StopStream sends the end command to stop a camera stream.
func (c *Controller) StopStream(ctx context.Context, deviceName string, sessionID [16]byte) error {
	c.mu.Lock()
	vc := c.verified[deviceName]
	active := c.activeStreams[deviceName]
	c.mu.Unlock()

	if vc == nil {
		return fmt.Errorf("device %q not verified", deviceName)
	}
	if active == nil {
		return fmt.Errorf("device %q has no active stream", deviceName)
	}

	// Send end command via SelectedRTPStreamConfiguration.
	endTLV := TLV8Encode([]TLV8Item{
		{Type: TLVSelectedSessionControl, Value: TLV8Encode([]TLV8Item{
			{Type: TLVSessionControlID, Value: sessionID[:]},
			{Type: TLVSessionControlCommand, Value: []byte{SessionCommandEnd}},
		})},
	})
	endB64 := base64.StdEncoding.EncodeToString(endTLV)

	err := vc.Client.PutCharacteristics([]Characteristic{
		{AID: active.aid, IID: active.selectedConfigIID, Value: endB64},
	})
	if err != nil {
		return fmt.Errorf("write end command: %w", err)
	}

	c.mu.Lock()
	delete(c.activeStreams, deviceName)
	c.mu.Unlock()

	c.logger.Info("stream stopped", "name", deviceName)
	return nil
}

// SubscribeMotionSensor subscribes to the MotionSensor.MotionDetected characteristic.
// The callback and context are stored so the subscription can be re-established
// after a HAP reconnection.
func (c *Controller) SubscribeMotionSensor(ctx context.Context, deviceName string, callback func(detected bool)) error {
	// Store for re-subscribe after reconnect.
	c.mu.Lock()
	c.motionCallbacks[deviceName] = callback
	c.motionCtx = ctx
	c.mu.Unlock()

	return c.doSubscribeMotion(ctx, deviceName, callback)
}

// doSubscribeMotion performs the actual motion subscription on the current
// verified connection. Called by SubscribeMotionSensor and by reconnect.
func (c *Controller) doSubscribeMotion(ctx context.Context, deviceName string, callback func(detected bool)) error {
	c.mu.Lock()
	vc := c.verified[deviceName]
	ids := c.charIDs[deviceName]
	c.mu.Unlock()

	if vc == nil {
		return fmt.Errorf("device %q not verified", deviceName)
	}
	if ids == nil || ids.motionDetectedIID == 0 {
		return fmt.Errorf("device %q has no motion sensor", deviceName)
	}

	// Subscribe to event notifications.
	if err := vc.Client.SubscribeCharacteristic(ids.motionDetectedAID, ids.motionDetectedIID); err != nil {
		return fmt.Errorf("subscribe motion: %w", err)
	}

	// Capture the current client so goroutines exit when this connection dies.
	client := vc.Client

	// Read events from the event channel in background.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-client.Events():
				if !ok {
					c.logger.Warn("motion event channel closed (will re-subscribe on reconnect)")
					return
				}
				c.logger.Debug("HAP event received", "characteristics", fmt.Sprintf("%+v", event.Characteristics))
				for _, ch := range event.Characteristics {
					if ch.AID == ids.motionDetectedAID && ch.IID == ids.motionDetectedIID {
						switch v := ch.Value.(type) {
						case bool:
							callback(v)
						case float64:
							callback(v != 0)
						default:
							c.logger.Warn("unexpected motion value type", "type", fmt.Sprintf("%T", ch.Value), "value", ch.Value)
						}
					}
				}
			}
		}
	}()

	// Also poll motion characteristic periodically as fallback,
	// since some cameras don't reliably push EVENT notifications.
	// Stops when the client's Done channel closes (connection lost) or ctx is cancelled.
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		var lastDetected *bool
		for {
			select {
			case <-ctx.Done():
				return
			case <-client.Done():
				c.logger.Debug("motion poll loop exiting (connection closed)")
				return
			case <-ticker.C:
				resp, err := client.GetCharacteristics(
					fmt.Sprintf("%d.%d", ids.motionDetectedAID, ids.motionDetectedIID),
				)
				if err != nil {
					c.logger.Debug("poll motion failed", "error", err)
					if isConnError(err) {
						c.logger.Debug("motion poll loop exiting (connection error)")
						return
					}
					continue
				}
				if len(resp.Characteristics) == 0 {
					continue
				}
				var detected bool
				switch v := resp.Characteristics[0].Value.(type) {
				case bool:
					detected = v
				case float64:
					detected = v != 0
				default:
					continue
				}
				if lastDetected == nil || *lastDetected != detected {
					c.logger.Info("motion poll", "detected", detected)
					lastDetected = &detected
					callback(detected)
				}
			}
		}
	}()

	c.logger.Info("subscribed to motion sensor", "name", deviceName)
	return nil
}

// findCameraCharIDs discovers the characteristic IIDs for camera streaming and motion.
func findCameraCharIDs(accessories *AccessoriesResponse) (*cameraCharIDs, error) {
	ids := &cameraCharIDs{}
	streamIndex := 0

	for _, acc := range accessories.Accessories {
		for _, svc := range acc.Services {
			svcType := strings.TrimPrefix(svc.Type, "public.hap.service.")
			// Also handle short UUID form (e.g., "110" or full UUID).
			svcTypeShort := trimHAPUUID(svcType)

			if svcTypeShort == ServiceCameraRTPStreamMgmt {
				streamIndex++
				streamIDs := cameraStreamCharIDs{
					index: streamIndex,
					aid:   acc.AID,
				}
				for _, ch := range svc.Characteristics {
					chType := trimHAPUUID(strings.TrimPrefix(ch.Type, "public.hap.characteristic."))
					switch chType {
					case CharSetupEndpoints:
						streamIDs.setupEndpointsIID = ch.IID
					case CharSelectedRTPConfig:
						streamIDs.selectedConfigIID = ch.IID
					case CharStreamingStatus:
						streamIDs.streamingStatusIID = ch.IID
					case CharSupportedVideoConfig:
						streamIDs.supportedVideoCfgIID = ch.IID
						streamIDs.videoConfig = parseSupportedVideoConfigValue(ch.Value)
					case CharSupportedAudioConfig:
						streamIDs.supportedAudioCfgIID = ch.IID
					}
				}
				if streamIDs.setupEndpointsIID != 0 && streamIDs.selectedConfigIID != 0 {
					ids.streams = append(ids.streams, streamIDs)
				}
			}

			if svcTypeShort == ServiceMotionSensor {
				for _, ch := range svc.Characteristics {
					chType := trimHAPUUID(strings.TrimPrefix(ch.Type, "public.hap.characteristic."))
					if chType == CharMotionDetected {
						ids.motionDetectedAID = acc.AID
						ids.motionDetectedIID = ch.IID
					}
				}
			}
		}
	}

	if len(ids.streams) == 0 {
		return nil, fmt.Errorf("camera streaming characteristics not found in accessory database")
	}

	return ids, nil
}

func loadSupportedVideoConfigs(client *HAPClient, ids *cameraCharIDs) error {
	var errs []string
	for i := range ids.streams {
		stream := &ids.streams[i]
		if stream.supportedVideoCfgIID == 0 {
			continue
		}
		resp, err := client.GetCharacteristics(
			fmt.Sprintf("%d.%d", stream.aid, stream.supportedVideoCfgIID),
		)
		if err != nil {
			errs = append(errs, fmt.Sprintf("channel %d: %v", stream.index, err))
			continue
		}
		if len(resp.Characteristics) == 0 {
			continue
		}
		cfg := parseSupportedVideoConfigValue(resp.Characteristics[0].Value)
		if len(cfg.Attributes) > 0 {
			stream.videoConfig = cfg
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func parseSupportedVideoConfigValue(value interface{}) SupportedVideoConfig {
	s, ok := value.(string)
	if !ok || s == "" {
		return SupportedVideoConfig{}
	}

	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return SupportedVideoConfig{}
	}

	items, err := TLV8Decode(data)
	if err != nil {
		return SupportedVideoConfig{}
	}

	var cfg SupportedVideoConfig
	for _, item := range items {
		if item.Type != TLVVideoCodecType {
			// SupportedVideoStreamConfiguration uses type 1 for each video
			// codec configuration. It intentionally overlaps with selected
			// stream sub-TLV constants.
			continue
		}
		codecItems, err := TLV8Decode(item.Value)
		if err != nil {
			continue
		}
		for _, codecItem := range codecItems {
			if codecItem.Type != TLVVideoAttributes {
				continue
			}
			attrItems, err := TLV8Decode(codecItem.Value)
			if err != nil {
				continue
			}
			attr := VideoAttribute{
				Width:  uint16FromTLV(attrItems, 0x01),
				Height: uint16FromTLV(attrItems, 0x02),
			}
			if fps, ok := TLV8GetByte(attrItems, 0x03); ok {
				attr.FPS = fps
			}
			if attr.Width != 0 && attr.Height != 0 {
				cfg.Attributes = append(cfg.Attributes, attr)
			}
		}
	}
	return cfg
}

func uint16FromTLV(items []TLV8Item, typ byte) uint16 {
	v := TLV8GetBytes(items, typ)
	if len(v) < 2 {
		return 0
	}
	return binary.LittleEndian.Uint16(v[:2])
}

func selectStreamOptions(streams []cameraStreamCharIDs, video VideoSelection, preferredChannel int) ([]streamStartOption, error) {
	if preferredChannel < 0 {
		return nil, fmt.Errorf("video.channel must be 0 or greater")
	}
	if preferredChannel > 0 {
		for _, stream := range streams {
			if stream.index == preferredChannel {
				return []streamStartOption{{
					ids:   stream,
					video: videoSelectionForStream(stream, video),
				}}, nil
			}
		}
		return nil, fmt.Errorf("video.channel %d not found; camera advertised %d stream channel(s)", preferredChannel, len(streams))
	}

	options := autoStreamOptions(streams, video)
	if len(options) > 0 {
		return options, nil
	}

	ordered := append([]cameraStreamCharIDs(nil), streams...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return streamMatchScore(ordered[i], video) < streamMatchScore(ordered[j], video)
	})
	for _, stream := range ordered {
		options = append(options, streamStartOption{
			ids:   stream,
			video: videoSelectionForStream(stream, video),
		})
	}
	return options, nil
}

func autoStreamOptions(streams []cameraStreamCharIDs, video VideoSelection) []streamStartOption {
	targetPixels := int(video.Width) * int(video.Height)
	bestByResolution := make(map[string]streamStartOption)

	for _, stream := range streams {
		for _, attr := range stream.videoConfig.Attributes {
			pixels := int(attr.Width) * int(attr.Height)
			if targetPixels > 0 && pixels > targetPixels {
				continue
			}
			optionVideo := video
			optionVideo.Width = attr.Width
			optionVideo.Height = attr.Height
			if attr.FPS != 0 {
				optionVideo.FPS = int(attr.FPS)
			}

			key := fmt.Sprintf("%dx%d@%d", optionVideo.Width, optionVideo.Height, optionVideo.FPS)
			existing, ok := bestByResolution[key]
			if !ok || streamPreferenceScore(stream, video) < streamPreferenceScore(existing.ids, video) {
				bestByResolution[key] = streamStartOption{
					ids:   stream,
					video: optionVideo,
				}
			}
		}
	}

	options := make([]streamStartOption, 0, len(bestByResolution))
	for _, option := range bestByResolution {
		options = append(options, option)
	}
	sort.SliceStable(options, func(i, j int) bool {
		iPixels := int(options[i].video.Width) * int(options[i].video.Height)
		jPixels := int(options[j].video.Width) * int(options[j].video.Height)
		if iPixels != jPixels {
			return iPixels > jPixels
		}
		if options[i].video.FPS != options[j].video.FPS {
			return options[i].video.FPS > options[j].video.FPS
		}
		return options[i].ids.index < options[j].ids.index
	})
	return options
}

func streamPreferenceScore(stream cameraStreamCharIDs, video VideoSelection) int {
	targetPixels := int(video.Width) * int(video.Height)
	maxPixels := maxStreamPixels(stream)
	if maxPixels == 0 {
		return stream.index + 1_000_000
	}
	score := absInt(maxPixels - targetPixels)
	if maxPixels > targetPixels {
		score += maxPixels - targetPixels
	}
	return score + stream.index
}

func maxStreamPixels(stream cameraStreamCharIDs) int {
	maxPixels := 0
	for _, attr := range stream.videoConfig.Attributes {
		pixels := int(attr.Width) * int(attr.Height)
		if pixels > maxPixels {
			maxPixels = pixels
		}
	}
	return maxPixels
}

func videoSelectionForStream(stream cameraStreamCharIDs, video VideoSelection) VideoSelection {
	if len(stream.videoConfig.Attributes) == 0 {
		return video
	}
	best := stream.videoConfig.Attributes[0]
	bestScore := attributeMatchScore(best, video)
	for _, attr := range stream.videoConfig.Attributes[1:] {
		score := attributeMatchScore(attr, video)
		if score < bestScore {
			best = attr
			bestScore = score
		}
	}
	video.Width = best.Width
	video.Height = best.Height
	if best.FPS != 0 {
		video.FPS = int(best.FPS)
	}
	return video
}

func streamMatchScore(stream cameraStreamCharIDs, video VideoSelection) int {
	if len(stream.videoConfig.Attributes) == 0 {
		return 1_000_000 + stream.index
	}

	best := int(^uint(0) >> 1)
	maxPixels := 0
	for _, attr := range stream.videoConfig.Attributes {
		pixels := int(attr.Width) * int(attr.Height)
		if pixels > maxPixels {
			maxPixels = pixels
		}
		score := attributeMatchScore(attr, video)
		if score < best {
			best = score
		}
	}
	targetPixels := int(video.Width) * int(video.Height)
	if maxPixels > targetPixels {
		best += (maxPixels - targetPixels) / 2000
	}
	return best
}

func attributeMatchScore(attr VideoAttribute, video VideoSelection) int {
	targetPixels := int(video.Width) * int(video.Height)
	pixels := int(attr.Width) * int(attr.Height)
	score := absInt(pixels-targetPixels) / 1000
	score += absInt(int(attr.Width)-int(video.Width))
	score += absInt(int(attr.Height)-int(video.Height))
	if video.FPS > 0 && attr.FPS > 0 {
		score += absInt(int(attr.FPS)-video.FPS) * 10
	}
	if pixels < targetPixels {
		score += 500
	}
	return score
}

func describeStreams(streams []cameraStreamCharIDs) string {
	var parts []string
	for _, stream := range streams {
		var attrs []string
		for _, attr := range stream.videoConfig.Attributes {
			attrs = append(attrs, fmt.Sprintf("%dx%d@%d", attr.Width, attr.Height, attr.FPS))
		}
		if len(attrs) == 0 {
			attrs = append(attrs, "unknown")
		}
		parts = append(parts, fmt.Sprintf("channel%d[%s]", stream.index, strings.Join(attrs, ",")))
	}
	return strings.Join(parts, " ")
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// trimHAPUUID extracts the short form from a full HAP UUID like "00000110-0000-1000-8000-0026BB765291".
func trimHAPUUID(s string) string {
	// If it's already a short form, return as-is.
	if !strings.Contains(s, "-") {
		return s
	}
	// Full HAP UUID format: XXXXXXXX-0000-1000-8000-0026BB765291
	// Extract the first segment and strip leading zeros.
	parts := strings.SplitN(s, "-", 2)
	short := strings.TrimLeft(parts[0], "0")
	if short == "" {
		short = "0"
	}
	return short
}

// Stop disconnects all devices and stops discovery.
func (c *Controller) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cancelFunc != nil {
		c.cancelFunc()
	}
	if c.controller != nil {
		c.controller.StopDiscovery()
	}

	for _, vc := range c.verified {
		if vc.Client != nil {
			vc.Client.Close()
		} else {
			vc.Conn.Close()
		}
	}
	for _, dev := range c.devices {
		dev.Close()
	}

	c.logger.Info("HAP controller stopped")
}

func randomUint32() uint32 {
	b := make([]byte, 4)
	rand.Read(b)
	return binary.LittleEndian.Uint32(b)
}
