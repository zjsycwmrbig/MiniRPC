package protocol

import (
	"encoding/binary"
	"errors"
	"io"
)

// Limits controls frame payload limits. Zero values select protocol defaults.
type Limits struct {
	MaxHeaderSize uint32
	MaxBodySize   uint32
}

// DefaultLimits returns the protocol v1 default frame limits.
func DefaultLimits() Limits {
	return Limits{
		MaxHeaderSize: DefaultMaxHeaderSize,
		MaxBodySize:   DefaultMaxBodySize,
	}
}

// Codec encodes and decodes Mini-RPC v1 frames.
//
// Codec is immutable after construction and safe for concurrent use.
type Codec struct {
	maxHeaderSize uint32
	maxBodySize   uint32
}

// DefaultCodec uses the protocol v1 default limits.
var DefaultCodec = mustNewCodec(DefaultLimits())

// NewCodec creates a codec with explicit limits. Zero limit fields select the
// default value. Header limits cannot exceed the v1 hard limit of 64 KiB.
func NewCodec(limits Limits) (*Codec, error) {
	if limits.MaxHeaderSize == 0 {
		limits.MaxHeaderSize = DefaultMaxHeaderSize
	}
	if limits.MaxBodySize == 0 {
		limits.MaxBodySize = DefaultMaxBodySize
	}
	if limits.MaxHeaderSize > DefaultMaxHeaderSize {
		return nil, newProtocolError(
			ErrInvalidLimits,
			"header limit %d exceeds v1 maximum %d",
			limits.MaxHeaderSize,
			DefaultMaxHeaderSize,
		)
	}
	maxInt := uint64(^uint(0) >> 1)
	if uint64(limits.MaxBodySize) > maxInt {
		return nil, newProtocolError(
			ErrInvalidLimits,
			"body limit %d exceeds platform allocation limit",
			limits.MaxBodySize,
		)
	}
	return &Codec{
		maxHeaderSize: limits.MaxHeaderSize,
		maxBodySize:   limits.MaxBodySize,
	}, nil
}

func mustNewCodec(limits Limits) *Codec {
	codec, err := NewCodec(limits)
	if err != nil {
		panic(err)
	}
	return codec
}

// Limits returns the immutable payload limits used by the codec.
func (codec *Codec) Limits() Limits {
	return Limits{
		MaxHeaderSize: codec.maxHeaderSize,
		MaxBodySize:   codec.maxBodySize,
	}
}

// Encode serializes one frame into a newly allocated byte slice.
//
// Encode never returns a buffer shared with Frame or another call, so callers
// may mutate the result without affecting concurrent encoding.
func (codec *Codec) Encode(frame Frame) ([]byte, error) {
	if !frame.Type.Valid() {
		return nil, newProtocolError(ErrInvalidFrameType, "%d", frame.Type)
	}
	if frame.Flags&^knownFlags != 0 {
		return nil, newProtocolError(
			ErrInvalidFlags,
			"reserved bits set in %#04x",
			frame.Flags,
		)
	}

	header, err := encodeMetadata(frame.Header, codec.maxHeaderSize)
	if err != nil {
		return nil, err
	}
	if uint64(len(header)) > uint64(codec.maxHeaderSize) {
		return nil, newProtocolError(
			ErrHeaderTooLarge,
			"%d bytes exceeds limit %d",
			len(header),
			codec.maxHeaderSize,
		)
	}
	if uint64(len(frame.Body)) > uint64(codec.maxBodySize) {
		return nil, newProtocolError(
			ErrBodyTooLarge,
			"%d bytes exceeds limit %d",
			len(frame.Body),
			codec.maxBodySize,
		)
	}

	encoded := make([]byte, FixedHeaderSize+len(header)+len(frame.Body))
	binary.BigEndian.PutUint16(encoded[0:2], Magic)
	encoded[2] = Version
	encoded[3] = byte(frame.Type)
	binary.BigEndian.PutUint16(encoded[4:6], frame.Flags)
	binary.BigEndian.PutUint32(encoded[6:10], uint32(len(header)))
	binary.BigEndian.PutUint32(encoded[10:14], uint32(len(frame.Body)))
	binary.BigEndian.PutUint64(encoded[14:22], frame.RequestID)
	copy(encoded[FixedHeaderSize:], header)
	copy(encoded[FixedHeaderSize+len(header):], frame.Body)
	return encoded, nil
}

// Write encodes and writes one complete frame, handling short writes.
func (codec *Codec) Write(writer io.Writer, frame Frame) error {
	encoded, err := codec.Encode(frame)
	if err != nil {
		return err
	}
	for len(encoded) > 0 {
		written, writeErr := writer.Write(encoded)
		if written < 0 || written > len(encoded) {
			return io.ErrShortWrite
		}
		encoded = encoded[written:]
		if writeErr != nil {
			return writeErr
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// Read decodes exactly one frame from reader.
//
// A clean EOF between frames is returned as io.EOF. EOF after any frame byte
// has been consumed is reported as ErrTruncatedFrame.
func (codec *Codec) Read(reader io.Reader) (*Frame, error) {
	var fixedHeader [FixedHeaderSize]byte
	read, err := io.ReadFull(reader, fixedHeader[:])
	if err != nil {
		if read == 0 && errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, newProtocolError(ErrTruncatedFrame, "fixed header: %v", err)
	}

	if magic := binary.BigEndian.Uint16(fixedHeader[0:2]); magic != Magic {
		return nil, newProtocolError(ErrInvalidMagic, "%#04x", magic)
	}
	if version := fixedHeader[2]; version != Version {
		return nil, newProtocolError(ErrUnsupportedVersion, "%d", version)
	}

	frameType := FrameType(fixedHeader[3])
	if !frameType.Valid() {
		return nil, newProtocolError(ErrInvalidFrameType, "%d", frameType)
	}

	flags := binary.BigEndian.Uint16(fixedHeader[4:6])
	if flags&^knownFlags != 0 {
		return nil, newProtocolError(ErrInvalidFlags, "reserved bits set in %#04x", flags)
	}

	headerLength := binary.BigEndian.Uint32(fixedHeader[6:10])
	if headerLength > codec.maxHeaderSize {
		return nil, newProtocolError(
			ErrHeaderTooLarge,
			"%d bytes exceeds limit %d",
			headerLength,
			codec.maxHeaderSize,
		)
	}
	bodyLength := binary.BigEndian.Uint32(fixedHeader[10:14])
	if bodyLength > codec.maxBodySize {
		return nil, newProtocolError(
			ErrBodyTooLarge,
			"%d bytes exceeds limit %d",
			bodyLength,
			codec.maxBodySize,
		)
	}

	headerBytes := make([]byte, int(headerLength))
	if err := readPayload(reader, headerBytes, "header"); err != nil {
		return nil, err
	}
	metadata, err := decodeMetadata(headerBytes)
	if err != nil {
		return nil, err
	}
	var body []byte
	if bodyLength > 0 {
		body = make([]byte, int(bodyLength))
		if err := readPayload(reader, body, "body"); err != nil {
			return nil, err
		}
	}

	return &Frame{
		Type:      frameType,
		Flags:     flags,
		RequestID: binary.BigEndian.Uint64(fixedHeader[14:22]),
		Header:    metadata,
		Body:      body,
	}, nil
}

func readPayload(reader io.Reader, payload []byte, field string) error {
	if len(payload) == 0 {
		return nil
	}
	read, err := io.ReadFull(reader, payload)
	if err != nil {
		return newProtocolError(
			ErrTruncatedFrame,
			"%s read %d of %d bytes: %v",
			field,
			read,
			len(payload),
			err,
		)
	}
	return nil
}

// ReadFrame reads one frame using DefaultCodec.
func ReadFrame(reader io.Reader) (*Frame, error) {
	return DefaultCodec.Read(reader)
}

// WriteFrame writes one frame using DefaultCodec.
func WriteFrame(writer io.Writer, frame Frame) error {
	return DefaultCodec.Write(writer, frame)
}
