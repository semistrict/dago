package serde

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

func marshalMessagePack(value any, limits Limits) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := msgpackEncoder{buffer: &buffer, limits: limits}
	if err := encoder.write(value, 0); err != nil {
		return nil, err
	}
	if buffer.Len() > limits.MaxBytes {
		return nil, fmt.Errorf("%w: payload exceeds %d bytes", ErrMalformedPayload, limits.MaxBytes)
	}
	return buffer.Bytes(), nil
}

type msgpackEncoder struct {
	buffer *bytes.Buffer
	limits Limits
}

func (encoder msgpackEncoder) write(value any, depth int) error {
	if depth > encoder.limits.MaxDepth {
		return fmt.Errorf("%w: maximum MessagePack depth exceeded", ErrMalformedPayload)
	}
	switch value := value.(type) {
	case nil:
		return encoder.buffer.WriteByte(0xc0)
	case bool:
		if value {
			return encoder.buffer.WriteByte(0xc3)
		}
		return encoder.buffer.WriteByte(0xc2)
	case string:
		return encoder.writeString(value)
	case []byte:
		return encoder.writeBytes(value)
	case int:
		return encoder.writeInt(int64(value))
	case int8:
		return encoder.writeInt(int64(value))
	case int16:
		return encoder.writeInt(int64(value))
	case int32:
		return encoder.writeInt(int64(value))
	case int64:
		return encoder.writeInt(value)
	case uint:
		return encoder.writeUint(uint64(value))
	case uint8:
		return encoder.writeUint(uint64(value))
	case uint16:
		return encoder.writeUint(uint64(value))
	case uint32:
		return encoder.writeUint(uint64(value))
	case uint64:
		return encoder.writeUint(value)
	case float32:
		if err := encoder.buffer.WriteByte(0xca); err != nil {
			return err
		}
		return binary.Write(encoder.buffer, binary.BigEndian, math.Float32bits(value))
	case float64:
		if err := encoder.buffer.WriteByte(0xcb); err != nil {
			return err
		}
		return binary.Write(encoder.buffer, binary.BigEndian, math.Float64bits(value))
	case []any:
		if err := encoder.writeLength(len(value), 0x90, 15, 0xdc, 0xdd); err != nil {
			return err
		}
		for _, item := range value {
			if err := encoder.write(item, depth+1); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		if err := encoder.writeLength(len(value), 0x80, 15, 0xde, 0xdf); err != nil {
			return err
		}
		for _, key := range sortedKeys(value) {
			if err := encoder.writeString(key); err != nil {
				return err
			}
			if err := encoder.write(value[key], depth+1); err != nil {
				return err
			}
		}
		return nil
	case extension:
		payload, err := marshalMessagePack(value.value, encoder.limits)
		if err != nil {
			return err
		}
		if len(payload) > encoder.limits.MaxBytes {
			return fmt.Errorf("%w: extension payload too large", ErrMalformedPayload)
		}
		if err := encoder.writeExtensionLength(len(payload)); err != nil {
			return err
		}
		if err := encoder.buffer.WriteByte(byte(value.code)); err != nil {
			return err
		}
		_, err = encoder.buffer.Write(payload)
		return err
	default:
		return fmt.Errorf("%w: MessagePack value %T", ErrUnsupportedType, value)
	}
}

func (encoder msgpackEncoder) writeInt(value int64) error {
	if value >= 0 {
		return encoder.writeUint(uint64(value))
	}
	switch {
	case value >= -32:
		return encoder.buffer.WriteByte(byte(int8(value)))
	case value >= math.MinInt8:
		if err := encoder.buffer.WriteByte(0xd0); err != nil {
			return err
		}
		return encoder.buffer.WriteByte(byte(int8(value)))
	case value >= math.MinInt16:
		if err := encoder.buffer.WriteByte(0xd1); err != nil {
			return err
		}
		return binary.Write(encoder.buffer, binary.BigEndian, int16(value))
	case value >= math.MinInt32:
		if err := encoder.buffer.WriteByte(0xd2); err != nil {
			return err
		}
		return binary.Write(encoder.buffer, binary.BigEndian, int32(value))
	default:
		if err := encoder.buffer.WriteByte(0xd3); err != nil {
			return err
		}
		return binary.Write(encoder.buffer, binary.BigEndian, value)
	}
}

func (encoder msgpackEncoder) writeUint(value uint64) error {
	switch {
	case value <= 0x7f:
		return encoder.buffer.WriteByte(byte(value))
	case value <= math.MaxUint8:
		if err := encoder.buffer.WriteByte(0xcc); err != nil {
			return err
		}
		return encoder.buffer.WriteByte(byte(value))
	case value <= math.MaxUint16:
		if err := encoder.buffer.WriteByte(0xcd); err != nil {
			return err
		}
		return binary.Write(encoder.buffer, binary.BigEndian, uint16(value))
	case value <= math.MaxUint32:
		if err := encoder.buffer.WriteByte(0xce); err != nil {
			return err
		}
		return binary.Write(encoder.buffer, binary.BigEndian, uint32(value))
	default:
		if err := encoder.buffer.WriteByte(0xcf); err != nil {
			return err
		}
		return binary.Write(encoder.buffer, binary.BigEndian, value)
	}
}

func (encoder msgpackEncoder) writeString(value string) error {
	length := len(value)
	if length > encoder.limits.MaxBytes {
		return fmt.Errorf("%w: string exceeds %d bytes", ErrMalformedPayload, encoder.limits.MaxBytes)
	}
	switch {
	case length <= 31:
		if err := encoder.buffer.WriteByte(0xa0 | byte(length)); err != nil {
			return err
		}
	case length <= math.MaxUint8:
		if err := encoder.buffer.WriteByte(0xd9); err != nil {
			return err
		}
		if err := encoder.buffer.WriteByte(byte(length)); err != nil {
			return err
		}
	case length <= math.MaxUint16:
		if err := encoder.buffer.WriteByte(0xda); err != nil {
			return err
		}
		if err := binary.Write(encoder.buffer, binary.BigEndian, uint16(length)); err != nil {
			return err
		}
	default:
		if err := encoder.buffer.WriteByte(0xdb); err != nil {
			return err
		}
		if err := binary.Write(encoder.buffer, binary.BigEndian, uint32(length)); err != nil {
			return err
		}
	}
	_, err := encoder.buffer.WriteString(value)
	return err
}

func (encoder msgpackEncoder) writeBytes(value []byte) error {
	length := len(value)
	if length > encoder.limits.MaxBytes {
		return fmt.Errorf("%w: bytes exceed %d bytes", ErrMalformedPayload, encoder.limits.MaxBytes)
	}
	switch {
	case length <= math.MaxUint8:
		if err := encoder.buffer.WriteByte(0xc4); err != nil {
			return err
		}
		if err := encoder.buffer.WriteByte(byte(length)); err != nil {
			return err
		}
	case length <= math.MaxUint16:
		if err := encoder.buffer.WriteByte(0xc5); err != nil {
			return err
		}
		if err := binary.Write(encoder.buffer, binary.BigEndian, uint16(length)); err != nil {
			return err
		}
	default:
		if err := encoder.buffer.WriteByte(0xc6); err != nil {
			return err
		}
		if err := binary.Write(encoder.buffer, binary.BigEndian, uint32(length)); err != nil {
			return err
		}
	}
	_, err := encoder.buffer.Write(value)
	return err
}

func (encoder msgpackEncoder) writeLength(length int, fixBase byte, fixMax int, code16, code32 byte) error {
	if length > encoder.limits.MaxCollection {
		return fmt.Errorf("%w: collection length %d", ErrMalformedPayload, length)
	}
	switch {
	case length <= fixMax:
		return encoder.buffer.WriteByte(fixBase | byte(length))
	case length <= math.MaxUint16:
		if err := encoder.buffer.WriteByte(code16); err != nil {
			return err
		}
		return binary.Write(encoder.buffer, binary.BigEndian, uint16(length))
	default:
		if err := encoder.buffer.WriteByte(code32); err != nil {
			return err
		}
		return binary.Write(encoder.buffer, binary.BigEndian, uint32(length))
	}
}

func (encoder msgpackEncoder) writeExtensionLength(length int) error {
	switch length {
	case 1:
		return encoder.buffer.WriteByte(0xd4)
	case 2:
		return encoder.buffer.WriteByte(0xd5)
	case 4:
		return encoder.buffer.WriteByte(0xd6)
	case 8:
		return encoder.buffer.WriteByte(0xd7)
	case 16:
		return encoder.buffer.WriteByte(0xd8)
	}
	if length <= math.MaxUint8 {
		if err := encoder.buffer.WriteByte(0xc7); err != nil {
			return err
		}
		return encoder.buffer.WriteByte(byte(length))
	}
	if length <= math.MaxUint16 {
		if err := encoder.buffer.WriteByte(0xc8); err != nil {
			return err
		}
		return binary.Write(encoder.buffer, binary.BigEndian, uint16(length))
	}
	if err := encoder.buffer.WriteByte(0xc9); err != nil {
		return err
	}
	return binary.Write(encoder.buffer, binary.BigEndian, uint32(length))
}

func unmarshalMessagePack(data []byte, limits Limits) (any, error) {
	decoder := msgpackDecoder{data: data, limits: limits}
	value, err := decoder.read(0)
	if err != nil {
		return nil, err
	}
	if decoder.offset != len(data) {
		return nil, fmt.Errorf("%w: trailing MessagePack data", ErrMalformedPayload)
	}
	return value, nil
}

type msgpackDecoder struct {
	data   []byte
	offset int
	limits Limits
}

func (decoder *msgpackDecoder) read(depth int) (any, error) {
	if depth > decoder.limits.MaxDepth {
		return nil, fmt.Errorf("%w: maximum MessagePack depth exceeded", ErrMalformedPayload)
	}
	code, err := decoder.byte()
	if err != nil {
		return nil, err
	}
	switch {
	case code <= 0x7f:
		return uint64(code), nil
	case code >= 0xe0:
		return int64(int8(code)), nil
	case code&0xe0 == 0xa0:
		return decoder.string(int(code & 0x1f))
	case code&0xf0 == 0x90:
		return decoder.array(int(code&0x0f), depth)
	case code&0xf0 == 0x80:
		return decoder.mapValue(int(code&0x0f), depth)
	}
	switch code {
	case 0xc0:
		return nil, nil
	case 0xc2:
		return false, nil
	case 0xc3:
		return true, nil
	case 0xc4:
		length, err := decoder.uint8()
		return decoder.bytes(int(length), err)
	case 0xc5:
		length, err := decoder.uint16()
		return decoder.bytes(int(length), err)
	case 0xc6:
		length, err := decoder.uint32()
		return decoder.bytes(int(length), err)
	case 0xca:
		bits, err := decoder.uint32()
		return float32(math.Float32frombits(bits)), err
	case 0xcb:
		bits, err := decoder.uint64()
		return math.Float64frombits(bits), err
	case 0xcc:
		value, err := decoder.uint8()
		return uint64(value), err
	case 0xcd:
		value, err := decoder.uint16()
		return uint64(value), err
	case 0xce:
		value, err := decoder.uint32()
		return uint64(value), err
	case 0xcf:
		return decoder.uint64()
	case 0xd0:
		value, err := decoder.uint8()
		return int64(int8(value)), err
	case 0xd1:
		value, err := decoder.uint16()
		return int64(int16(value)), err
	case 0xd2:
		value, err := decoder.uint32()
		return int64(int32(value)), err
	case 0xd3:
		value, err := decoder.uint64()
		return int64(value), err
	case 0xd9:
		length, err := decoder.uint8()
		if err != nil {
			return nil, err
		}
		return decoder.string(int(length))
	case 0xda:
		length, err := decoder.uint16()
		if err != nil {
			return nil, err
		}
		return decoder.string(int(length))
	case 0xdb:
		length, err := decoder.uint32()
		if err != nil {
			return nil, err
		}
		return decoder.string(int(length))
	case 0xdc:
		length, err := decoder.uint16()
		if err != nil {
			return nil, err
		}
		return decoder.array(int(length), depth)
	case 0xdd:
		length, err := decoder.uint32()
		if err != nil {
			return nil, err
		}
		return decoder.array(int(length), depth)
	case 0xde:
		length, err := decoder.uint16()
		if err != nil {
			return nil, err
		}
		return decoder.mapValue(int(length), depth)
	case 0xdf:
		length, err := decoder.uint32()
		if err != nil {
			return nil, err
		}
		return decoder.mapValue(int(length), depth)
	case 0xd4:
		return decoder.extension(1, depth)
	case 0xd5:
		return decoder.extension(2, depth)
	case 0xd6:
		return decoder.extension(4, depth)
	case 0xd7:
		return decoder.extension(8, depth)
	case 0xd8:
		return decoder.extension(16, depth)
	case 0xc7:
		length, err := decoder.uint8()
		if err != nil {
			return nil, err
		}
		return decoder.extension(int(length), depth)
	case 0xc8:
		length, err := decoder.uint16()
		if err != nil {
			return nil, err
		}
		return decoder.extension(int(length), depth)
	case 0xc9:
		length, err := decoder.uint32()
		if err != nil {
			return nil, err
		}
		return decoder.extension(int(length), depth)
	default:
		return nil, fmt.Errorf("%w: MessagePack code 0x%x", ErrUnsupportedEncoding, code)
	}
}

func (decoder *msgpackDecoder) array(length, depth int) ([]any, error) {
	if err := decoder.collection(length); err != nil {
		return nil, err
	}
	result := make([]any, length)
	for index := range result {
		value, err := decoder.read(depth + 1)
		if err != nil {
			return nil, err
		}
		result[index] = value
	}
	return result, nil
}

func (decoder *msgpackDecoder) mapValue(length, depth int) (map[string]any, error) {
	if err := decoder.collection(length); err != nil {
		return nil, err
	}
	result := make(map[string]any, length)
	for range length {
		key, err := decoder.read(depth + 1)
		if err != nil {
			return nil, err
		}
		keyString, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("%w: non-string map key %T", ErrUnsupportedEncoding, key)
		}
		value, err := decoder.read(depth + 1)
		if err != nil {
			return nil, err
		}
		result[keyString] = value
	}
	return result, nil
}

func (decoder *msgpackDecoder) extension(length, depth int) (any, error) {
	code, err := decoder.byte()
	if err != nil {
		return nil, err
	}
	payload, err := decoder.take(length)
	if err != nil {
		return nil, err
	}
	if int8(code) != DeltaSnapshotExtension {
		return nil, fmt.Errorf("%w: MessagePack extension %d", ErrUnsupportedEncoding, int8(code))
	}
	value, err := unmarshalMessagePack(payload, decoder.limits)
	if err != nil {
		return nil, err
	}
	return extension{code: int8(code), value: value}, nil
}

func (decoder *msgpackDecoder) collection(length int) error {
	if length < 0 || length > decoder.limits.MaxCollection {
		return fmt.Errorf("%w: collection length %d", ErrMalformedPayload, length)
	}
	return nil
}

func (decoder *msgpackDecoder) string(length int) (string, error) {
	data, err := decoder.take(length)
	return string(data), err
}

func (decoder *msgpackDecoder) bytes(length int, prior error) ([]byte, error) {
	if prior != nil {
		return nil, prior
	}
	data, err := decoder.take(length)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), data...), nil
}

func (decoder *msgpackDecoder) byte() (byte, error) {
	data, err := decoder.take(1)
	if err != nil {
		return 0, err
	}
	return data[0], nil
}

func (decoder *msgpackDecoder) uint8() (uint8, error) {
	value, err := decoder.byte()
	return uint8(value), err
}

func (decoder *msgpackDecoder) uint16() (uint16, error) {
	data, err := decoder.take(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(data), nil
}

func (decoder *msgpackDecoder) uint32() (uint32, error) {
	data, err := decoder.take(4)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(data), nil
}

func (decoder *msgpackDecoder) uint64() (uint64, error) {
	data, err := decoder.take(8)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(data), nil
}

func (decoder *msgpackDecoder) take(length int) ([]byte, error) {
	if length < 0 || length > decoder.limits.MaxBytes || decoder.offset+length > len(decoder.data) {
		return nil, fmt.Errorf("%w: truncated or oversized MessagePack value", ErrMalformedPayload)
	}
	data := decoder.data[decoder.offset : decoder.offset+length]
	decoder.offset += length
	return data, nil
}
