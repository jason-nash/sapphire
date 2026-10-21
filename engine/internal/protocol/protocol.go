package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const MaxMessage = 4 << 20

type Request struct {
	Action        string          `json:"action"`
	Name          string          `json:"name,omitempty"`
	Destination   string          `json:"destination,omitempty"`
	Configuration json.RawMessage `json:"configuration,omitempty"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
}

type Response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

func Success(value any) Response {
	data, err := json.Marshal(value)
	if err != nil {
		return Failure(err)
	}
	return Response{OK: true, Result: data}
}

func Failure(err error) Response {
	return Response{Result: json.RawMessage("null"), Error: err.Error()}
}

func Decode(reader io.Reader, value any) error {
	data, err := io.ReadAll(io.LimitReader(reader, MaxMessage+1))
	if err != nil {
		return err
	}
	if len(data) > MaxMessage {
		return errors.New("JSON message exceeds 4 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

func (r Request) Validate() error {
	if r.Action == "" || strings.TrimSpace(r.Action) != r.Action || len(r.Action) > 128 {
		return errors.New("action must be a nonempty name of at most 128 bytes")
	}
	if len(r.Name) > 63 || len(r.Destination) > 63 {
		return errors.New("name or destination exceeds 63 bytes")
	}
	return nil
}

func (r Response) Err() error {
	if !r.OK {
		if r.Error == "" {
			return errors.New("guest request failed")
		}
		return fmt.Errorf("%s", r.Error)
	}
	return nil
}
