package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
)

func TestCodecRoundTrip(t *testing.T) {
	t.Parallel()

	codec := mustNewCodec(DefaultLimits())
	original := Frame{
		Type:      FrameTypeRequest,
		Flags:     FlagCompressed | FlagOneWay,
		RequestID: 42,
		Header: map[string]string{
			MetadataKeyService: "echo-1",
			MetadataKeyMethod:  "Echo",
		},
		Body: []byte{0x08, 0x01, 0x12, 0x03, 'r', 'p', 'c'},
	}

	encoded, err := codec.Encode(original)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	decoded, err := codec.Read(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if !reflect.DeepEqual(decoded, &original) {
		t.Fatalf("decoded frame = %#v, want %#v", decoded, original)
	}
}

func TestEncodeFixedHeaderUsesBigEndian(t *testing.T) {
	t.Parallel()

	codec := mustNewCodec(DefaultLimits())
	encoded, err := codec.Encode(Frame{
		Type:      FrameTypeRequest,
		RequestID: 42,
	})
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	const wantHex = "4d52010100000000000000000000000000000000002a"
	if got := hex.EncodeToString(encoded); got != wantHex {
		t.Fatalf("encoded frame = %s, want %s", got, wantHex)
	}
}

func TestEncodeMetadataIsDeterministic(t *testing.T) {
	t.Parallel()

	codec := mustNewCodec(DefaultLimits())
	first, err := codec.Encode(Frame{
		Type: FrameTypeRequest,
		Header: map[string]string{
			MetadataKeyService: "echo-1",
			MetadataKeyMethod:  "Echo",
		},
	})
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	second, err := codec.Encode(Frame{
		Type: FrameTypeRequest,
		Header: map[string]string{
			MetadataKeyMethod:  "Echo",
			MetadataKeyService: "echo-1",
		},
	})
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("equivalent metadata maps produced different frames")
	}

	const wantHeaderHex = "000000066d6574686f64000000044563686f0000000773657276696365000000066563686f2d31"
	headerLength := binary.BigEndian.Uint32(first[6:10])
	if got := hex.EncodeToString(first[FixedHeaderSize : FixedHeaderSize+int(headerLength)]); got != wantHeaderHex {
		t.Fatalf("metadata = %s, want %s", got, wantHeaderHex)
	}
}

func TestReadHandlesSplitHeaderAndPayload(t *testing.T) {
	t.Parallel()

	codec := mustNewCodec(DefaultLimits())
	frame := Frame{
		Type:      FrameTypeResponse,
		RequestID: 99,
		Header:    map[string]string{MetadataKeySpanID: "abc"},
		Body:      []byte("response"),
	}
	encoded, err := codec.Encode(frame)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	decoded, err := codec.Read(&oneByteReader{reader: bytes.NewReader(encoded)})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if !reflect.DeepEqual(decoded, &frame) {
		t.Fatalf("decoded frame = %#v, want %#v", decoded, frame)
	}
}

func TestReadHandlesMultipleFrames(t *testing.T) {
	t.Parallel()

	codec := mustNewCodec(DefaultLimits())
	frames := []Frame{
		{Type: FrameTypeRequest, RequestID: 1, Body: []byte("first")},
		{Type: FrameTypeResponse, RequestID: 1, Body: []byte("second")},
		{Type: FrameTypeCancel, RequestID: 2},
		{Type: FrameTypeHeartbeat, RequestID: 0},
	}

	var stream bytes.Buffer
	for _, frame := range frames {
		if err := codec.Write(&stream, frame); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}

	for index, want := range frames {
		got, err := codec.Read(&stream)
		if err != nil {
			t.Fatalf("Read() frame %d error = %v", index, err)
		}
		if !reflect.DeepEqual(got, &want) {
			t.Fatalf("frame %d = %#v, want %#v", index, got, want)
		}
	}
	if _, err := codec.Read(&stream); !errors.Is(err, io.EOF) {
		t.Fatalf("final Read() error = %v, want io.EOF", err)
	}
}

func TestWriteHandlesShortWrites(t *testing.T) {
	t.Parallel()

	codec := mustNewCodec(DefaultLimits())
	frame := Frame{Type: FrameTypeRequest, RequestID: 7, Body: []byte("short writes")}
	writer := &shortWriter{max: 3}

	if err := codec.Write(writer, frame); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	decoded, err := codec.Read(bytes.NewReader(writer.Bytes()))
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if !reflect.DeepEqual(decoded, &frame) {
		t.Fatalf("decoded frame = %#v, want %#v", decoded, frame)
	}
}

func TestWriteRejectsZeroLengthWrite(t *testing.T) {
	t.Parallel()

	err := mustNewCodec(DefaultLimits()).Write(zeroWriter{}, Frame{Type: FrameTypeRequest})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Write() error = %v, want io.ErrShortWrite", err)
	}
}

func TestReadRejectsInvalidFixedFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		edit func([]byte)
		want error
	}{
		{
			name: "magic",
			edit: func(encoded []byte) {
				binary.BigEndian.PutUint16(encoded[0:2], 0xFFFF)
			},
			want: ErrInvalidMagic,
		},
		{
			name: "version",
			edit: func(encoded []byte) {
				encoded[2] = 2
			},
			want: ErrUnsupportedVersion,
		},
		{
			name: "type",
			edit: func(encoded []byte) {
				encoded[3] = 99
			},
			want: ErrInvalidFrameType,
		},
		{
			name: "flags",
			edit: func(encoded []byte) {
				binary.BigEndian.PutUint16(encoded[4:6], 1<<15)
			},
			want: ErrInvalidFlags,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			codec := mustNewCodec(DefaultLimits())
			encoded, err := codec.Encode(Frame{Type: FrameTypeRequest})
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			test.edit(encoded)

			if _, err := codec.Read(bytes.NewReader(encoded)); !errors.Is(err, test.want) {
				t.Fatalf("Read() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestReadRejectsOversizedLengths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		edit func([]byte)
		want error
	}{
		{
			name: "header",
			edit: func(encoded []byte) {
				binary.BigEndian.PutUint32(encoded[6:10], DefaultMaxHeaderSize+1)
			},
			want: ErrHeaderTooLarge,
		},
		{
			name: "body",
			edit: func(encoded []byte) {
				binary.BigEndian.PutUint32(encoded[10:14], DefaultMaxBodySize+1)
			},
			want: ErrBodyTooLarge,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			codec := mustNewCodec(DefaultLimits())
			encoded, err := codec.Encode(Frame{Type: FrameTypeRequest})
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			test.edit(encoded)

			if _, err := codec.Read(bytes.NewReader(encoded)); !errors.Is(err, test.want) {
				t.Fatalf("Read() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestEncodeEnforcesConfiguredLimits(t *testing.T) {
	t.Parallel()

	codec, err := NewCodec(Limits{MaxHeaderSize: 16, MaxBodySize: 2})
	if err != nil {
		t.Fatalf("NewCodec() error = %v", err)
	}

	if _, err := codec.Encode(Frame{
		Type:   FrameTypeRequest,
		Header: map[string]string{"long-key": "long-value"},
	}); !errors.Is(err, ErrHeaderTooLarge) {
		t.Fatalf("Encode() header error = %v, want ErrHeaderTooLarge", err)
	}
	if _, err := codec.Encode(Frame{
		Type: FrameTypeRequest,
		Body: []byte{1, 2, 3},
	}); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Encode() body error = %v, want ErrBodyTooLarge", err)
	}
}

func TestNewCodecRejectsInvalidLimits(t *testing.T) {
	t.Parallel()

	if _, err := NewCodec(Limits{MaxHeaderSize: DefaultMaxHeaderSize + 1}); !errors.Is(err, ErrInvalidLimits) {
		t.Fatalf("NewCodec() error = %v, want ErrInvalidLimits", err)
	}
}

func TestReadRejectsTruncatedFrames(t *testing.T) {
	t.Parallel()

	codec := mustNewCodec(DefaultLimits())
	encoded, err := codec.Encode(Frame{
		Type:   FrameTypeRequest,
		Header: map[string]string{MetadataKeyService: "echo"},
		Body:   []byte("body"),
	})
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	tests := []struct {
		name  string
		input []byte
	}{
		{name: "fixed header", input: encoded[:10]},
		{name: "metadata", input: encoded[:FixedHeaderSize+1]},
		{name: "body", input: encoded[:len(encoded)-1]},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := codec.Read(bytes.NewReader(test.input)); !errors.Is(err, ErrTruncatedFrame) {
				t.Fatalf("Read() error = %v, want ErrTruncatedFrame", err)
			}
		})
	}
}

func TestReadReturnsCleanEOFBetweenFrames(t *testing.T) {
	t.Parallel()

	if _, err := mustNewCodec(DefaultLimits()).Read(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("Read() error = %v, want io.EOF", err)
	}
}

func TestReadRejectsMalformedMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header []byte
		want   error
	}{
		{
			name:   "truncated key length",
			header: []byte{0, 0},
			want:   ErrMalformedMetadata,
		},
		{
			name:   "truncated key",
			header: appendUint32(1),
			want:   ErrMalformedMetadata,
		},
		{
			name:   "empty key",
			header: append(appendUint32(0), appendUint32(0)...),
			want:   ErrEmptyMetadataKey,
		},
		{
			name:   "truncated value length",
			header: metadataEntry("key", "")[:9],
			want:   ErrMalformedMetadata,
		},
		{
			name:   "truncated value",
			header: append(metadataEntry("key", "value"), appendUint32(2)...),
			want:   ErrMalformedMetadata,
		},
		{
			name:   "duplicate key",
			header: append(metadataEntry("key", "one"), metadataEntry("key", "two")...),
			want:   ErrDuplicateMetadataKey,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			encoded := rawFrame(FrameTypeRequest, test.header, nil)
			if _, err := mustNewCodec(DefaultLimits()).Read(bytes.NewReader(encoded)); !errors.Is(err, test.want) {
				t.Fatalf("Read() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestEncodeRejectsEmptyMetadataKey(t *testing.T) {
	t.Parallel()

	_, err := mustNewCodec(DefaultLimits()).Encode(Frame{
		Type:   FrameTypeRequest,
		Header: map[string]string{"": "value"},
	})
	if !errors.Is(err, ErrEmptyMetadataKey) {
		t.Fatalf("Encode() error = %v, want ErrEmptyMetadataKey", err)
	}
}

func TestConcurrentEncodingDoesNotShareBuffers(t *testing.T) {
	t.Parallel()

	const workers = 64
	codec := mustNewCodec(DefaultLimits())
	encodedFrames := make([][]byte, workers)
	errs := make([]error, workers)
	var waitGroup sync.WaitGroup
	waitGroup.Add(workers)

	for worker := 0; worker < workers; worker++ {
		worker := worker
		go func() {
			defer waitGroup.Done()
			encodedFrames[worker], errs[worker] = codec.Encode(Frame{
				Type:      FrameTypeRequest,
				RequestID: uint64(worker + 1),
				Body:      []byte{byte(worker)},
			})
		}()
	}
	waitGroup.Wait()

	for worker := 0; worker < workers; worker++ {
		if errs[worker] != nil {
			t.Fatalf("worker %d Encode() error = %v", worker, errs[worker])
		}
		if requestID := binary.BigEndian.Uint64(encodedFrames[worker][14:22]); requestID != uint64(worker+1) {
			t.Fatalf("worker %d request ID = %d, want %d", worker, requestID, worker+1)
		}
		encodedFrames[worker][0] = 0
	}
}

func TestFrameTypeString(t *testing.T) {
	t.Parallel()

	tests := map[FrameType]string{
		FrameTypeRequest:   "request",
		FrameTypeResponse:  "response",
		FrameTypeCancel:    "cancel",
		FrameTypeHeartbeat: "heartbeat",
		FrameType(99):      "unknown",
	}
	for frameType, want := range tests {
		if got := frameType.String(); got != want {
			t.Errorf("FrameType(%d).String() = %q, want %q", frameType, got, want)
		}
	}
}

type oneByteReader struct {
	reader io.Reader
}

func (reader *oneByteReader) Read(destination []byte) (int, error) {
	if len(destination) > 1 {
		destination = destination[:1]
	}
	return reader.reader.Read(destination)
}

type shortWriter struct {
	bytes.Buffer
	max int
}

func (writer *shortWriter) Write(source []byte) (int, error) {
	if len(source) > writer.max {
		source = source[:writer.max]
	}
	return writer.Buffer.Write(source)
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) {
	return 0, nil
}

func rawFrame(frameType FrameType, header []byte, body []byte) []byte {
	encoded := make([]byte, FixedHeaderSize+len(header)+len(body))
	binary.BigEndian.PutUint16(encoded[0:2], Magic)
	encoded[2] = Version
	encoded[3] = byte(frameType)
	binary.BigEndian.PutUint32(encoded[6:10], uint32(len(header)))
	binary.BigEndian.PutUint32(encoded[10:14], uint32(len(body)))
	copy(encoded[FixedHeaderSize:], header)
	copy(encoded[FixedHeaderSize+len(header):], body)
	return encoded
}

func metadataEntry(key string, value string) []byte {
	encoded := appendUint32String(nil, key)
	return appendUint32String(encoded, value)
}

func appendUint32(value uint32) []byte {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	return encoded[:]
}
