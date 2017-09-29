package pgtype

import (
	"database/sql/driver"
	"encoding/json"

	"github.com/pkg/errors"
)

type JSON struct {
	Bytes  []byte
	Status Status
}

func (dst *JSON) Set(src interface{}) error {
	if src == nil {
		*dst = JSON{Status: Null}
		return nil
	}

	switch value := src.(type) {
	case string:
		*dst = JSON{Bytes: []byte(value), Status: Present}
	case *string:
		if value == nil {
			*dst = JSON{Status: Null}
		} else {
			*dst = JSON{Bytes: []byte(*value), Status: Present}
		}
	case []byte:
		if value == nil {
			*dst = JSON{Status: Null}
		} else {
			*dst = JSON{Bytes: value, Status: Present}
		}
	default:
		buf, err := json.Marshal(value)
		if err != nil {
			return err
		}
		*dst = JSON{Bytes: buf, Status: Present}
	}

	return nil
}

func (dst *JSON) Get() interface{} {
	switch dst.Status {
	case Present:
		var i interface{}
		err := json.Unmarshal(dst.Bytes, &i)
		if err != nil {
			return dst
		}
		return i
	case Null:
		return nil
	default:
		return dst.Status
	}
}

func (src *JSON) AssignTo(dst interface{}) error {
	switch v := dst.(type) {
	case *string:
		if src.Status != Present {
			v = nil
		} else {
			*v = string(src.Bytes)
		}
	case **string:
		*v = new(string)
		return src.AssignTo(*v)
	case *[]byte:
		if src.Status != Present {
			*v = nil
		} else {
			buf := make([]byte, len(src.Bytes))
			copy(buf, src.Bytes)
			*v = buf
		}
	default:
		data := src.Bytes
		if data == nil || src.Status != Present {
			data = []byte("null")
		}

		return json.Unmarshal(data, dst)
	}

	return nil
}

func (dst *JSON) DecodeText(ci *ConnInfo, src []byte) error {
	if src == nil {
		*dst = JSON{Status: Null}
		return nil
	}

	*dst = JSON{Bytes: src, Status: Present}
	return nil
}

func (dst *JSON) DecodeBinary(ci *ConnInfo, src []byte) error {
	return dst.DecodeText(ci, src)
}

func (src *JSON) EncodeText(ci *ConnInfo, buf []byte) ([]byte, error) {
	switch src.Status {
	case Null:
		return nil, nil
	case Undefined:
		return nil, errUndefined
	}

	return append(buf, src.Bytes...), nil
}

func (src *JSON) EncodeBinary(ci *ConnInfo, buf []byte) ([]byte, error) {
	return src.EncodeText(ci, buf)
}

// Scan implements the database/sql Scanner interface.
func (dst *JSON) Scan(src interface{}) error {
	if src == nil {
		*dst = JSON{Status: Null}
		return nil
	}

	switch src := src.(type) {
	case string:
		return dst.DecodeText(nil, []byte(src))
	case []byte:
		srcCopy := make([]byte, len(src))
		copy(srcCopy, src)
		return dst.DecodeText(nil, srcCopy)
	}

	return errors.Errorf("cannot scan %T", src)
}

// Value implements the database/sql/driver Valuer interface.
func (src *JSON) Value() (driver.Value, error) {
	switch src.Status {
	case Present:
		return string(src.Bytes), nil
	case Null:
		return nil, nil
	default:
		return nil, errUndefined
	}
}
