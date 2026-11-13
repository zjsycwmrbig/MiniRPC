package protocol

const (
	// Magic identifies a Mini-RPC byte stream.
	Magic uint16 = 0x4D52

	// Version is the current wire protocol version.
	Version uint8 = 1

	// FixedHeaderSize is the encoded size of all fields before Metadata.
	FixedHeaderSize = 22

	// DefaultMaxHeaderSize is the hard v1 limit for encoded Metadata.
	DefaultMaxHeaderSize uint32 = 64 << 10

	// DefaultMaxBodySize is the default limit for an encoded Protobuf body.
	DefaultMaxBodySize uint32 = 8 << 20
)

// FrameType identifies the operation carried by a frame.
type FrameType uint8

const (
	FrameTypeRequest   FrameType = 1
	FrameTypeResponse  FrameType = 2
	FrameTypeCancel    FrameType = 3
	FrameTypeHeartbeat FrameType = 4
)

// Valid reports whether the frame type is defined by protocol v1.
func (frameType FrameType) Valid() bool {
	switch frameType {
	case FrameTypeRequest, FrameTypeResponse, FrameTypeCancel, FrameTypeHeartbeat:
		return true
	default:
		return false
	}
}

// String returns a stable name for logs and errors.
func (frameType FrameType) String() string {
	switch frameType {
	case FrameTypeRequest:
		return "request"
	case FrameTypeResponse:
		return "response"
	case FrameTypeCancel:
		return "cancel"
	case FrameTypeHeartbeat:
		return "heartbeat"
	default:
		return "unknown"
	}
}

const (
	// FlagCompressed means Body contains a compressed payload.
	FlagCompressed uint16 = 1 << 0

	// FlagError means Body or Metadata describes a failed RPC.
	FlagError uint16 = 1 << 1

	// FlagOneWay marks a request that does not expect a response.
	FlagOneWay uint16 = 1 << 2
)

const knownFlags = FlagCompressed | FlagError | FlagOneWay

// Frame is one complete Mini-RPC protocol frame.
//
// RequestID is scoped to a single connection. Header contains encoded
// Metadata keys and values. Body is owned by the frame after decoding.
type Frame struct {
	Type      FrameType
	Flags     uint16
	RequestID uint64
	Header    map[string]string
	Body      []byte
}
