package goavro

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"time"
)

const (
	// CompressionNullLabel is used when OCF blocks are not compressed.
	CompressionNullLabel = "null"

	// CompressionDeflateLabel is used when OCF blocks are compressed using the
	// deflate algorithm.
	CompressionDeflateLabel = "deflate"

	// CompressionSnappyLabel is used when OCF blocks are compressed using the
	// snappy algorithm.
	CompressionSnappyLabel = "snappy"
)

// compressionID are values used to specify compression algorithm used to compress
// and decompress Avro Object Container File (OCF) streams.
type compressionID uint8

const (
	compressionNull compressionID = iota
	compressionDeflate
	compressionSnappy
)

const (
	ocfBlockConst      = 24 // Each OCF block has two longs prefix, and sync marker suffix
	ocfHeaderSizeConst = 48 // OCF header is usually about 48 bytes longer than its compressed schema
	ocfMagicString     = "Obj\x01"
	ocfMetadataSchema  = `{"type":"map","values":"bytes"}`
	ocfSyncLength      = 16
)

var (
	ocfMagicBytes    = []byte(ocfMagicString)
	ocfMetadataCodec *Codec
)

func init() {
	ocfMetadataCodec, _ = NewCodec(ocfMetadataSchema)
}

type ocfHeader struct {
	codec         *Codec
	compressionID compressionID
	syncMarker    []byte
}

func newOCFHeader(config OCFConfig) (*ocfHeader, error) {
	var err error

	header := new(ocfHeader)

	//
	// avro.codec
	//
	switch config.CompressionName {
	case "":
		header.compressionID = compressionNull
	case CompressionNullLabel:
		header.compressionID = compressionNull
	case CompressionDeflateLabel:
		header.compressionID = compressionDeflate
	case CompressionSnappyLabel:
		header.compressionID = compressionSnappy
	default:
		return nil, fmt.Errorf("cannot create OCF header using unrecognized compression algorithm: %q", config.CompressionName)
	}

	//
	// avro.schema
	//
	if config.Codec != nil {
		header.codec = config.Codec
	} else if config.Schema == "" {
		return nil, fmt.Errorf("cannot create OCF header without either Codec or Schema specified")
	} else {
		if header.codec, err = NewCodec(config.Schema); err != nil {
			return nil, fmt.Errorf("cannot create OCF header: %s", err)
		}
	}

	//
	// The 16-byte, randomly-generated sync marker for this file.
	//
	r := rand.New(rand.NewSource(time.Now().Unix()))
	header.syncMarker = make([]byte, ocfSyncLength)
	for i := 0; i < ocfSyncLength; i++ {
		header.syncMarker[i] = byte(r.Intn(256))
	}

	return header, nil
}

func readOCFHeader(ior io.Reader) (*ocfHeader, error) {
	//
	// magic bytes
	//
	magic := make([]byte, 4)
	_, err := io.ReadFull(ior, magic)
	if err != nil {
		return nil, fmt.Errorf("cannot read OCF header magic bytes: %s", err)
	}
	if bytes.Compare(magic, ocfMagicBytes) != 0 {
		return nil, fmt.Errorf("cannot read OCF header with invalid magic bytes: %#q", magic)
	}

	//
	// metadata
	//
	metadata, err := metadataBinaryReader(ior)
	if err != nil {
		return nil, fmt.Errorf("cannot read OCF header metadata: %s", err)
	}

	//
	// avro.codec
	//
	// NOTE: Avro specification states that `null` cID is used by
	// default when "avro.codec" was not included in the metadata header. The
	// specification does not talk about the case when "avro.codec" was included
	// with the empty string as its value. I believe it is an error for an OCF
	// file to provide the empty string as the cID algorithm. While it
	// is trivially easy to gracefully handle here, I'm not sure whether this
	// happens a lot, and don't want to accept bad input unless we have
	// significant reason to do so.
	var cID compressionID
	value, ok := metadata["avro.codec"]
	if ok {
		switch avroCodec := string(value); avroCodec {
		case CompressionNullLabel:
			cID = compressionNull
		case CompressionDeflateLabel:
			cID = compressionDeflate
		case CompressionSnappyLabel:
			cID = compressionSnappy
		default:
			return nil, fmt.Errorf("cannot read OCF header using unrecognized compression algorithm from avro.codec: %q", avroCodec)
		}
	}

	//
	// create goavro.Codec from specified avro.schema
	//
	value, ok = metadata["avro.schema"]
	if !ok {
		return nil, errors.New("cannot read OCF header without avro.schema")
	}
	codec, err := NewCodec(string(value))
	if err != nil {
		return nil, fmt.Errorf("cannot read OCF header with invalid avro.schema: %s", err)
	}

	//
	// read and store sync marker
	//
	syncMarker := make([]byte, ocfSyncLength)
	n, err := io.ReadAtLeast(ior, syncMarker, ocfSyncLength)
	if err != nil {
		return nil, fmt.Errorf("cannot read OCF header without sync marker: only read %d of %d bytes: %s", n, ocfSyncLength, err)
	}

	//
	// header is valid
	//
	return &ocfHeader{codec: codec, compressionID: cID, syncMarker: syncMarker}, nil
}

func writeOCFHeader(header *ocfHeader, iow io.Writer) (err error) {
	//
	// avro.codec
	//
	var avroCodec string
	switch header.compressionID {
	case compressionNull:
		avroCodec = CompressionNullLabel
	case compressionDeflate:
		avroCodec = CompressionDeflateLabel
	case compressionSnappy:
		avroCodec = CompressionSnappyLabel
	default:
		return fmt.Errorf("should not get here: cannot write OCF header using unrecognized compression algorithm: %d", header.compressionID)
	}

	//
	// avro.schema
	//
	// Create buffer for OCF header. The first four bytes are magic, and we'll
	// use copy to fill them in, so initialize buffer's length with 4, and its
	// capacity equal to length of avro schema plus a constant.
	schema := header.codec.Schema()
	buf := make([]byte, 4, len(schema)+ocfHeaderSizeConst)
	_ = copy(buf, ocfMagicBytes)

	//
	// file metadata, including the schema
	//
	buf, err = ocfMetadataCodec.BinaryFromNative(buf, map[string]interface{}{"avro.schema": []byte(schema), "avro.codec": []byte(avroCodec)})
	if err != nil {
		return fmt.Errorf("should not get here: cannot write OCF header: %s", err)
	}

	//
	// 16-byte sync marker
	//
	buf = append(buf, header.syncMarker...)

	// emit OCF header
	_, err = iow.Write(buf)
	if err != nil {
		return fmt.Errorf("cannot write OCF header: %s", err)
	}
	return nil
}
