package integration

import (
	"testing"

	"github.com/container2wasm/container2wasm/tests/integration/utils"
)

// Tests of the WASI 0.3 component output (`c2w --target=wasi-p3`).
// `wasmtime-p3` is wasmtime >= 46 installed by tests/Dockerfile.d/Dockerfile.runner.
func TestWasmtimeP3(t *testing.T) {
	utils.RunTestRuntimes(t, []utils.TestSpec{
		{
			Name:    "wasmtime-p3-hello",
			Runtime: "wasmtime-p3",
			Inputs: []utils.Input{
				{Image: "alpine:3.17", ConvertOpts: []string{"--target=wasi-p3"}, Architecture: utils.X8664},
			},
			RuntimeOpts: utils.StringFlags("run", "-Sp3"),
			Args:        utils.StringFlags("echo", "-n", "hello"),
			Want:        utils.WantString("hello"),
		},
		// Interactive stdin is not exercised: wasi-libc's non-blocking reads of
		// stdin block and report end of file on WASI 0.3 (see docs/wasip3.md),
		// so the console stops polling stdin after the first read.
		{
			Name:    "wasmtime-p3-env",
			Runtime: "wasmtime-p3",
			Inputs: []utils.Input{
				{Image: "alpine:3.17", ConvertOpts: []string{"--target=wasi-p3"}, Architecture: utils.X8664},
			},
			RuntimeOpts: utils.StringFlags("run", "-Sp3", "--env=AAA=hello", "--env=BBB=world"),
			Args:        utils.StringFlags("/bin/sh", "-c", "echo -n $AAA $BBB"),
			Want:        utils.WantString("hello world"),
		},
		{
			// The bridge: wasi:clocks/system-clock of the host is called from
			// inside the container through /run/c2w/c2w-call.
			Name:    "wasmtime-p3-bridge-clock",
			Runtime: "wasmtime-p3",
			Inputs: []utils.Input{
				{Image: "alpine:3.17", ConvertOpts: []string{"--target=wasi-p3", "--wit=" + utils.AssetPath + "tests/wit/clock"}, Architecture: utils.X8664},
			},
			RuntimeOpts: utils.StringFlags("run", "-Sp3"),
			Args: utils.StringFlags("/bin/sh", "-c",
				"/run/c2w/c2w-call wasi:clocks/system-clock@0.3.0 now | grep -o seconds && /run/c2w/c2w-call --describe | head -c 1"),
			Want: utils.WantString("seconds\n{"),
		},
	}...)
}
