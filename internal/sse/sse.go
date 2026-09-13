// Package sse 按事件读取 Server-Sent Events 流，同时保留每个事件的原始字节，便于原样转发。
package sse

import (
	"bufio"
	"bytes"
	"io"
)

// Event 是流里的一个事件。
type Event struct {
	Raw  []byte // 原始字节，包括结尾的空行，转发时原样写出
	Data []byte // 所有 data 行的值，多行之间用换行连接；注释等不含 data 的事件为空
}

// Reader 从流里逐个读出事件。换行只认 \n 和 \r\n，OpenAI 兼容的上游都用这两种。
type Reader struct {
	br *bufio.Reader
}

// NewReader 返回一个从 r 读取事件的 Reader。
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReader(r)}
}

// Next 返回下一个事件。流正常结束时返回 io.EOF；最后一个事件缺少结尾空行时按 SSE 规范丢弃，同样返回 io.EOF。
// 其他错误（例如连接中途断开）原样返回。
func (r *Reader) Next() (Event, error) {
	var raw []byte
	var data [][]byte
	for {
		line, err := r.br.ReadBytes('\n')
		if err != nil {
			return Event{}, err
		}
		content := bytes.TrimRight(line, "\r\n")
		if len(content) == 0 {
			if len(raw) == 0 {
				continue // 事件之间多余的空行
			}
			raw = append(raw, line...)
			return Event{Raw: raw, Data: bytes.Join(data, []byte("\n"))}, nil
		}
		raw = append(raw, line...)
		if value, ok := bytes.CutPrefix(content, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(value, []byte(" ")))
		}
	}
}
