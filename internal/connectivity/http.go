package connectivity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type RequestError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
	Cause      error
}

func (e *RequestError) Error() string { return e.Message }
func (e *RequestError) Unwrap() error { return e.Cause }

func Request(ctx context.Context, client *http.Client, method, base, path, token string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return &RequestError{Method: method, Path: path, Message: "无法连接所选中继，请检查地址和网络", Cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &RequestError{
			Method:     method,
			Path:       path,
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("中继拒绝请求（HTTP %d）；请检查配对码是否已使用、过期或用途错误", resp.StatusCode),
		}
	}
	if output != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(output); err != nil {
			return &RequestError{Method: method, Path: path, StatusCode: resp.StatusCode, Message: "所选地址未返回 Vantry 中继数据，请确认使用原生中继入口", Cause: err}
		}
		return nil
	}
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return err
}
