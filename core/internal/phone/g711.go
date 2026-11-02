package phone

// G.711 A-law (RTP payload type 8, "PCMA", Europe) and µ-law (0, "PCMU"): 8 kHz, one byte per
// sample. The classic ITU-T reference algorithms, as devices/atoms3r/src/phone/g711.h.

var alawSegments = [8]int{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}

func encodeAlaw(pcm int16) byte {
	sample := int(pcm) >> 3 // 13 bits
	mask := byte(0xD5)
	if sample < 0 {
		mask = 0x55
		sample = -sample - 1
	}
	segment := 0
	for segment < 8 && sample > alawSegments[segment] {
		segment++
	}
	if segment >= 8 {
		return 0x7F ^ mask
	}
	value := byte(segment << 4)
	if segment < 2 {
		value |= byte(sample>>1) & 0x0F
	} else {
		value |= byte(sample>>segment) & 0x0F
	}
	return value ^ mask
}

func decodeAlaw(alaw byte) int16 {
	alaw ^= 0x55
	value := int(alaw&0x0F) << 4
	segment := int(alaw&0x70) >> 4
	switch segment {
	case 0:
		value += 8
	case 1:
		value += 0x108
	default:
		value = (value + 0x108) << (segment - 1)
	}
	if alaw&0x80 != 0 {
		return int16(value)
	}
	return int16(-value)
}

func encodeUlaw(pcm int16) byte {
	const bias, clip = 0x84, 32635
	sample := int(pcm)
	var sign byte
	if sample < 0 {
		sign = 0x80
		sample = -sample
	}
	if sample > clip {
		sample = clip
	}
	sample += bias
	exponent := 7
	for mask := 0x4000; exponent > 0 && sample&mask == 0; mask >>= 1 {
		exponent--
	}
	mantissa := (sample >> (exponent + 3)) & 0x0F
	return ^(sign | byte(exponent<<4) | byte(mantissa))
}

func decodeUlaw(ulaw byte) int16 {
	ulaw = ^ulaw
	exponent := int(ulaw>>4) & 0x07
	value := (int(ulaw&0x0F)<<3 + 0x84) << exponent
	if ulaw&0x80 != 0 {
		return int16(0x84 - value)
	}
	return int16(value - 0x84)
}

// Payload types of the codecs we speak.
const (
	payloadPCMU = 0
	payloadPCMA = 8
)

func encodeSample(payload uint8, sample int16) byte {
	if payload == payloadPCMU {
		return encodeUlaw(sample)
	}
	return encodeAlaw(sample)
}

func decodeSample(payload uint8, code byte) int16 {
	if payload == payloadPCMU {
		return decodeUlaw(code)
	}
	return decodeAlaw(code)
}
