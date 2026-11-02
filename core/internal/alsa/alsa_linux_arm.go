package alsa

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// ioctl numbers: _IOWR/_IOW/_IOR/_IO with the 32-bit ARM structure sizes below.
const (
	ioctlHwParams  = 0xC25C4111 // SNDRV_PCM_IOCTL_HW_PARAMS, struct snd_pcm_hw_params (604)
	ioctlPrepare   = 0x00004140 // SNDRV_PCM_IOCTL_PREPARE
	ioctlDrop      = 0x00004143 // SNDRV_PCM_IOCTL_DROP
	ioctlWriteI    = 0x400C4150 // SNDRV_PCM_IOCTL_WRITEI_FRAMES, struct snd_xferi (12)
	ioctlReadI     = 0x800C4151 // SNDRV_PCM_IOCTL_READI_FRAMES
	ioctlElemInfo  = 0xC1105511 // SNDRV_CTL_IOCTL_ELEM_INFO, struct snd_ctl_elem_info (272)
	ioctlElemRead  = 0xC2C85512 // SNDRV_CTL_IOCTL_ELEM_READ, struct snd_ctl_elem_value (712)
	ioctlElemWrite = 0xC2C85513 // SNDRV_CTL_IOCTL_ELEM_WRITE
)

// struct snd_pcm_hw_params: flags, 3 masks and 5 reserved masks of 32 bytes, 12 intervals and 9
// reserved ones of 12 bytes (min, max, flags), then rmask, cmask, info, ...
const (
	hwParamsSize        = 604
	hwMasks             = 4
	hwIntervals         = 4 + 8*32
	hwRmask             = hwIntervals + 21*12
	hwInfo              = hwRmask + 8
	paramAccess         = 0
	paramFormat         = 1
	paramSubformat      = 2
	paramChannels       = 10
	paramRate           = 11
	paramPeriod         = 13 // PERIOD_SIZE in frames
	paramPeriods        = 15
	paramBuffer         = 17 // BUFFER_SIZE in frames
	accessRWInterleaved = 3
	intervalInteger     = 1 << 2
)

// PCM is one open stream (playback or capture), blocking reads and writes of whole frames.
type PCM struct {
	f          *os.File
	frameBytes int
	Period     int // frames per period, as the driver set them
	Buffer     int // frames in the ring buffer
	Xruns      int // underruns (playback) or overruns (capture) so far
}

func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	for {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
		if errno == syscall.EINTR { // the Go runtime's preemption signals
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}

// OpenPCM opens /dev/snd/pcmC<card>D<device>p (or c) with the given configuration.
func OpenPCM(card, device int, capture bool, c Config) (*PCM, error) {
	dir := 'p'
	if capture {
		dir = 'c'
	}
	path := fmt.Sprintf("/dev/snd/pcmC%dD%d%c", card, device, dir)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var p [hwParamsSize]byte
	le := binary.LittleEndian
	for i := 0; i < 3; i++ {
		for w := 0; w < 8; w++ {
			le.PutUint32(p[hwMasks+i*32+w*4:], ^uint32(0))
		}
	}
	for i := 0; i < 12; i++ {
		le.PutUint32(p[hwIntervals+i*12+4:], ^uint32(0))
	}
	le.PutUint32(p[hwRmask:], ^uint32(0))
	le.PutUint32(p[hwInfo:], ^uint32(0))
	mask := func(param, bit int) {
		o := hwMasks + param*32
		for w := 0; w < 8; w++ {
			le.PutUint32(p[o+w*4:], 0)
		}
		le.PutUint32(p[o+(bit/32)*4:], 1<<(bit%32))
	}
	exact := func(param, v int) {
		o := hwIntervals + (param-8)*12
		le.PutUint32(p[o:], uint32(v))
		le.PutUint32(p[o+4:], uint32(v))
		le.PutUint32(p[o+8:], intervalInteger)
	}
	mask(paramAccess, accessRWInterleaved)
	mask(paramFormat, int(c.Format))
	mask(paramSubformat, 0)
	exact(paramChannels, c.Channels)
	exact(paramRate, c.Rate)
	exact(paramPeriod, c.PeriodFrames)
	exact(paramPeriods, c.Periods)
	if err := ioctl(f.Fd(), ioctlHwParams, unsafe.Pointer(&p[0])); err != nil {
		f.Close()
		return nil, fmt.Errorf("alsa: %s: hw params (%d ch, %d Hz, format %d, %dx%d): %w", path, c.Channels, c.Rate, c.Format, c.PeriodFrames, c.Periods, err)
	}
	pcm := &PCM{f: f, frameBytes: c.Channels * c.Format.Bytes(),
		Period: int(le.Uint32(p[hwIntervals+(paramPeriod-8)*12:])),
		Buffer: int(le.Uint32(p[hwIntervals+(paramBuffer-8)*12:]))}
	if err := ioctl(f.Fd(), ioctlPrepare, nil); err != nil {
		f.Close()
		return nil, fmt.Errorf("alsa: %s: prepare: %w", path, err)
	}
	return pcm, nil
}

// xferi: struct snd_xferi (result, buf, frames), 32-bit.
type xferi struct {
	result int32
	buf    uintptr
	frames uint32
}

// transfer moves all of buf (whole frames). An xrun (EPIPE: the playback ran dry, the capture
// overflowed) prepares the stream again and goes on; Xruns counts them.
func (p *PCM) transfer(req uintptr, buf []byte) error {
	for len(buf) >= p.frameBytes {
		x := xferi{buf: uintptr(unsafe.Pointer(&buf[0])), frames: uint32(len(buf) / p.frameBytes)}
		err := ioctl(p.f.Fd(), req, unsafe.Pointer(&x))
		runtime.KeepAlive(buf)
		if errors.Is(err, syscall.EPIPE) {
			p.Xruns++
			if err := ioctl(p.f.Fd(), ioctlPrepare, nil); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if x.result <= 0 {
			continue
		}
		buf = buf[int(x.result)*p.frameBytes:]
	}
	return nil
}

// Write plays interleaved frames; blocks while the ring buffer is full.
func (p *PCM) Write(buf []byte) error { return p.transfer(ioctlWriteI, buf) }

// Read fills buf with interleaved frames; blocks until they were captured.
func (p *PCM) Read(buf []byte) error { return p.transfer(ioctlReadI, buf) }

// Close stops the stream at once (drops what is queued).
func (p *PCM) Close() error {
	ioctl(p.f.Fd(), ioctlDrop, nil)
	return p.f.Close()
}

// --- Mixer controls -----------------------------------------------------------------

// struct snd_ctl_elem_id (64 bytes): numid, iface, device, subdevice, name[44], index.
const (
	idName       = 16
	idNameLen    = 44
	ifaceMixer   = 2
	infoSize     = 272
	infoType     = 64
	infoCount    = 72
	infoItems    = 80 // value.enumerated.items
	infoItem     = 84 // value.enumerated.item (in: which name to fill)
	infoItemName = 88
	valueSize    = 712
	valueData    = 72 // value union (8-byte aligned after id and the indirect bit field)
	valueBytes   = 512
)

// Mixer: the card's control device.
type Mixer struct{ f *os.File }

func OpenMixer(card int) (*Mixer, error) {
	f, err := os.OpenFile(fmt.Sprintf("/dev/snd/controlC%d", card), os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &Mixer{f: f}, nil
}

func (m *Mixer) Close() error { return m.f.Close() }

func setID(buf []byte, name string) error {
	if len(name) >= idNameLen {
		return fmt.Errorf("alsa: control name too long: %s", name)
	}
	binary.LittleEndian.PutUint32(buf[4:], ifaceMixer)
	copy(buf[idName:idName+idNameLen], name)
	return nil
}

func (m *Mixer) info(name string) (controlInfo, error) {
	var b [infoSize]byte
	if err := setID(b[:], name); err != nil {
		return controlInfo{}, err
	}
	if err := ioctl(m.f.Fd(), ioctlElemInfo, unsafe.Pointer(&b[0])); err != nil {
		return controlInfo{}, fmt.Errorf("alsa: control %q: %w", name, err)
	}
	le := binary.LittleEndian
	info := controlInfo{typ: int(le.Uint32(b[infoType:])), count: int(le.Uint32(b[infoCount:]))}
	if info.typ == typeEnumerated {
		items := int(le.Uint32(b[infoItems:]))
		for i := 0; i < items && i < 64; i++ {
			le.PutUint32(b[infoItem:], uint32(i))
			if err := ioctl(m.f.Fd(), ioctlElemInfo, unsafe.Pointer(&b[0])); err != nil {
				return controlInfo{}, fmt.Errorf("alsa: control %q item %d: %w", name, i, err)
			}
			n := b[infoItemName : infoItemName+64]
			if z := indexZero(n); z >= 0 {
				n = n[:z]
			}
			info.items = append(info.items, string(n))
		}
	}
	return info, nil
}

func indexZero(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

// Set writes a boolean, integer or enumerated control by name (as amixer cset): one value for
// all channels or one per channel.
func (m *Mixer) Set(name string, values ...string) error {
	info, err := m.info(name)
	if err != nil {
		return err
	}
	nums, err := parseValues(name, info, values)
	if err != nil {
		return err
	}
	var b [valueSize]byte
	setID(b[:], name)
	for i, v := range nums {
		binary.LittleEndian.PutUint32(b[valueData+4*i:], uint32(v))
	}
	if err := ioctl(m.f.Fd(), ioctlElemWrite, unsafe.Pointer(&b[0])); err != nil {
		return fmt.Errorf("alsa: set %q: %w", name, err)
	}
	return nil
}

// SetBytes writes a BYTES control (a coefficient blob).
func (m *Mixer) SetBytes(name string, data []byte) error {
	info, err := m.info(name)
	if err != nil {
		return err
	}
	if info.typ != typeBytes || len(data) != info.count || len(data) > valueBytes {
		return fmt.Errorf("alsa: %s: %d bytes for a control of type %d with %d", name, len(data), info.typ, info.count)
	}
	var b [valueSize]byte
	setID(b[:], name)
	copy(b[valueData:], data)
	if err := ioctl(m.f.Fd(), ioctlElemWrite, unsafe.Pointer(&b[0])); err != nil {
		return fmt.Errorf("alsa: set %q: %w", name, err)
	}
	return nil
}

// Get reads a control's values (enumerations as item numbers, BYTES as bytes).
func (m *Mixer) Get(name string) ([]int32, error) {
	info, err := m.info(name)
	if err != nil {
		return nil, err
	}
	var b [valueSize]byte
	setID(b[:], name)
	if err := ioctl(m.f.Fd(), ioctlElemRead, unsafe.Pointer(&b[0])); err != nil {
		return nil, fmt.Errorf("alsa: get %q: %w", name, err)
	}
	out := make([]int32, info.count)
	for i := range out {
		if info.typ == typeBytes {
			out[i] = int32(b[valueData+i])
		} else {
			out[i] = int32(binary.LittleEndian.Uint32(b[valueData+4*i:]))
		}
	}
	return out, nil
}
