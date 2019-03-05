package main

import (
	"os"
	"path/filepath"

	"github.com/xiaonanln/goworld/engine/config"
)

// Env represents environment variables
type Env struct {
	GoWorldRoot string
}

// GetDispatcherDir returns the path to the dispatcher
func (env *Env) GetDispatcherDir() string {
	return filepath.Join(env.GoWorldRoot, "components", "dispatcher")
}

// GetGateDir returns the path to the gate
func (env *Env) GetGateDir() string {
	return filepath.Join(env.GoWorldRoot, "components", "gate")
}

// GetDispatcherBinary returns the path to the dispatcher binary
func (env *Env) GetDispatcherBinary() string {
	return filepath.Join(env.GetDispatcherDir(), "dispatcher"+BinaryExtension)
}

// GetGateBinary returns the path to the gate binary
func (env *Env) GetGateBinary() string {
	return filepath.Join(env.GetGateDir(), "gate"+BinaryExtension)
}

var env Env

// 探测goworld.ini所在目录
func detectGoWorldPath() {
	goworldPath, _ := filepath.Abs(filepath.Dir(os.Args[0]))
	configFile := filepath.Join(goworldPath, "goworld.ini")
	if !isexists(configFile) {
		return
	}

	env.GoWorldRoot = goworldPath
	showMsg("goworld directory found: %s", env.GoWorldRoot)
	config.SetConfigFile(configFile)
}
