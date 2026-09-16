// ═══ 更新日志 ═══
// 2026-09-16：为请求重写提供保留数字原值的完整 JSON 解码，避免大整数精度丢失及尾随内容漏检。
// Package jsonutil provides lossless JSON number decoding for request transformations.
package jsonutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Decode reads exactly one JSON value and preserves numbers stored in interfaces
// as json.Number. Typed numeric fields retain encoding/json's usual validation.
func Decode(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values are not allowed")
	}
	return nil
}
