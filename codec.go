package cache

import (
	"encoding/json"
	"fmt"
)

type Codec interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
}

type JSONCodec struct{}

func (JSONCodec) Marshal(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("cache: json marshal: %w", err)
	}
	return data, nil
}

func (JSONCodec) Unmarshal(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("cache: json unmarshal: %w", err)
	}
	return nil
}
