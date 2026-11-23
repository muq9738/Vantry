package relaycmd

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/muq9738/Vantry/internal/connectivity"
)

func initConfig(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("vantry-relay init", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("config", "", "新建的私有配置文件，不覆盖已有文件")
	listen := flags.String("listen", "127.0.0.1:8787", "监听地址")
	certificate := flags.String("certificate", "", "TLS 证书文件")
	privateKey := flags.String("private-key", "", "TLS 私钥文件")
	desktop := flags.Bool("desktop", false, "同时启用独立桌面中继")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *path == "" || flags.NArg() != 0 {
		return errors.New("init 需要 --config 指定新配置文件")
	}
	cfg := Config{Listen: *listen, Certificate: *certificate, PrivateKey: *privateKey, Sessions: &connectivity.RelayConfig{RegistrationKey: connectivity.Token(), Routes: map[string]string{}}}
	if *desktop {
		cfg.Desktop = &struct {
			RegistrationKey string `json:"registrationKey"`
		}{RegistrationKey: connectivity.Token()}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(*path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	_, err = fmt.Fprintf(output, "已创建中继配置 %s（权限 0600）。\n", *path)
	return err
}
