package protocol

import (
	"encoding/binary"
	"sort"
)

const metadataLengthSize = 4

func encodeMetadata(metadata map[string]string, maxHeaderSize uint32) ([]byte, error) {
	if len(metadata) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(metadata))
	size := uint64(0)
	for key, value := range metadata {
		if key == "" {
			return nil, newProtocolError(ErrEmptyMetadataKey, "metadata key cannot be empty")
		}
		if uint64(len(key)) > uint64(^uint32(0)) || uint64(len(value)) > uint64(^uint32(0)) {
			return nil, newProtocolError(ErrHeaderTooLarge, "metadata string exceeds uint32 length")
		}
		keys = append(keys, key)
		size += uint64(metadataLengthSize + len(key) + metadataLengthSize + len(value))
		if size > uint64(maxHeaderSize) {
			return nil, newProtocolError(
				ErrHeaderTooLarge,
				"estimated %d bytes exceeds limit %d",
				size,
				maxHeaderSize,
			)
		}
	}
	sort.Strings(keys)

	encoded := make([]byte, 0, int(size))
	for _, key := range keys {
		value := metadata[key]
		encoded = appendUint32String(encoded, key)
		encoded = appendUint32String(encoded, value)
	}
	return encoded, nil
}

func decodeMetadata(encoded []byte) (map[string]string, error) {
	if len(encoded) == 0 {
		return nil, nil
	}

	metadata := make(map[string]string)
	for offset := 0; offset < len(encoded); {
		key, next, err := readUint32String(encoded, offset, "metadata key")
		if err != nil {
			return nil, err
		}
		if key == "" {
			return nil, newProtocolError(ErrEmptyMetadataKey, "metadata key at offset %d is empty", offset)
		}
		value, nextValue, err := readUint32String(encoded, next, "metadata value")
		if err != nil {
			return nil, err
		}
		if _, exists := metadata[key]; exists {
			return nil, newProtocolError(ErrDuplicateMetadataKey, "%q at offset %d", key, offset)
		}
		metadata[key] = value
		offset = nextValue
	}
	return metadata, nil
}

func appendUint32String(destination []byte, value string) []byte {
	var length [metadataLengthSize]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	destination = append(destination, length[:]...)
	return append(destination, value...)
}

func readUint32String(encoded []byte, offset int, field string) (string, int, error) {
	if len(encoded)-offset < metadataLengthSize {
		return "", 0, newProtocolError(
			ErrMalformedMetadata,
			"%s length at offset %d is truncated",
			field,
			offset,
		)
	}

	length := binary.BigEndian.Uint32(encoded[offset : offset+metadataLengthSize])
	offset += metadataLengthSize
	if uint64(length) > uint64(len(encoded)-offset) {
		return "", 0, newProtocolError(
			ErrMalformedMetadata,
			"%s at offset %d declares %d bytes with %d remaining",
			field,
			offset-metadataLengthSize,
			length,
			len(encoded)-offset,
		)
	}
	end := offset + int(length)
	return string(encoded[offset:end]), end, nil
}
