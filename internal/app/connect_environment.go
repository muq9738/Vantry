package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/muq9738/Vantry/internal/desktop"
	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/pkg/api"
)

func ConnectEnvironment(ctx context.Context, dataDir, connectionID string) (*desktop.EnvironmentSession, error) {
	client, err := remoteaccess.Connect(ctx, remoteaccess.Options{DataDir: dataDir}, connectionID)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*desktop.EnvironmentSession, error) { _ = client.Close(); return nil, err }
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	required := RequiredRemoteDesktopCapabilities()
	if client.Protocol == "t3" {
		required = RequiredCoreCapabilities()
	}
	if _, err := VerifyCoreCapabilities(probe, client, required); err != nil {
		return fail(err)
	}
	response, err := client.Workbench(probe, api.WorkbenchRequest{Action: "environment.info"})
	if err != nil {
		return fail(err)
	}
	var info api.EnvironmentInfo
	if json.Unmarshal([]byte(response.Output), &info) != nil || info.ID == "" {
		return fail(errors.New("目标机器没有返回执行环境信息"))
	}
	return &desktop.EnvironmentSession{Service: client, Info: info, Close: client.Close, Mode: client.Mode}, nil
}
