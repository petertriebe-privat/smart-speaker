//go:build !(linux && arm)

package alsa

// The structures in alsa_linux_arm.go are the 32-bit ARM ones: elsewhere (the PC) nothing opens.

type PCM struct{ Period, Buffer, Xruns int }

func OpenPCM(card, device int, capture bool, c Config) (*PCM, error) { return nil, ErrUnsupported }
func (p *PCM) Write(buf []byte) error                                { return ErrUnsupported }
func (p *PCM) Read(buf []byte) error                                 { return ErrUnsupported }
func (p *PCM) Close() error                                          { return nil }

type Mixer struct{}

func OpenMixer(card int) (*Mixer, error)                 { return nil, ErrUnsupported }
func (m *Mixer) Close() error                            { return nil }
func (m *Mixer) Set(name string, values ...string) error { return ErrUnsupported }
func (m *Mixer) SetBytes(name string, data []byte) error { return ErrUnsupported }
func (m *Mixer) Get(name string) ([]int32, error)        { return nil, ErrUnsupported }
