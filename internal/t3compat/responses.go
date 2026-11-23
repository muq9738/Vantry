package t3compat

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/muq9738/Vantry/pkg/api"
)

func (a *RemoteAdapter) response(ctx context.Context, command api.Command) (Object, error) {
	if command.Cancelled {
		return Object{"decision": "cancel"}, nil
	}
	projection, _, err := a.detail(ctx, command.ThreadID)
	if err != nil {
		return nil, err
	}
	kind := ""
	for _, raw := range rows(projection["runtimeRequests"]) {
		request := object(raw)
		if text(request, "id") == command.RequestID {
			kind = text(request, "kind")
			break
		}
	}
	if kind == "" {
		return nil, errors.New("该 T3 请求已不存在或不属于此会话")
	}
	if kind != "user-input" && kind != "user_input" {
		decision := "decline"
		if command.Confirmed {
			decision = "accept"
		}
		return Object{"decision": decision}, nil
	}
	for _, raw := range rows(projection["turnItems"]) {
		item := object(raw)
		if text(item, "requestId") != command.RequestID {
			continue
		}
		questions := rows(item["questions"])
		if len(questions) == 1 {
			question := object(questions[0])
			value := command.Value
			for _, rawOption := range rows(question["options"]) {
				option := object(rawOption)
				if text(option, "label") == value && text(option, "value") != "" {
					value = text(option, "value")
					break
				}
			}
			return Object{"answers": Object{text(question, "id"): Object{"answers": []string{value}}}}, nil
		}
		if len(questions) > 1 {
			var answers Object
			if json.Unmarshal([]byte(command.Value), &answers) != nil {
				return nil, errors.New("此请求有多道问题，请在编辑框中填写各项 JSON 答案")
			}
			for _, rawQuestion := range questions {
				question := object(rawQuestion)
				if question["required"] == true {
					if _, ok := answers[text(question, "id")]; !ok {
						return nil, errors.New("尚有必填问题未回答")
					}
				}
			}
			return Object{"answers": answers}, nil
		}
	}
	return nil, errors.New("T3 未提供此问题的可回答内容，请在 T3 中处理")
}
