package backend

import (
	"strings"
	"testing"
)

// Lines captured from llama.cpp b12182 loading a model over two RPC servers
// with --tensor-split 1,3 and --log-verbosity 4.
const realLoadLog = `0.00.900.111 I load_tensors: offloading output layer to GPU
0.00.900.112 I load_tensors: offloading 29 repeating layers to GPU
0.00.900.113 I load_tensors: offloaded 31/31 layers to GPU
0.00.900.114 I load_tensors:   CPU_Mapped model buffer size =    28.69 MiB
0.00.900.115 I load_tensors: RPC0[127.0.0.1:50551] model buffer size =    28.72 MiB
0.00.900.116 I load_tensors: RPC0[127.0.0.1:50552] model buffer size =   107.68 MiB
0.01.100.000 I llama_kv_cache: RPC0[127.0.0.1:50551] KV buffer size =     6.38 MiB
0.01.100.001 I llama_kv_cache: RPC0[127.0.0.1:50552] KV buffer size =    17.53 MiB
0.01.200.000 I sched_reserve: RPC0[127.0.0.1:50551] compute buffer size =    29.93 MiB
0.01.200.001 I sched_reserve: RPC0[127.0.0.1:50552] compute buffer size =    19.79 MiB
0.01.200.002 I sched_reserve:        CPU compute buffer size =    15.32 MiB
0.01.200.003 I llama_context:        CPU  output buffer size =     0.38 MiB
`

func TestAllocationFromRealLog(t *testing.T) {
	var a Allocation
	// Arbitrary write boundaries, as a pipe delivers them.
	for _, chunk := range []string{realLoadLog[:37], realLoadLog[37:401], realLoadLog[401:]} {
		if _, err := a.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	devices, offloaded := a.Devices()
	if offloaded != "31/31" || len(devices) != 3 {
		t.Fatalf("%q %+v", offloaded, devices)
	}
	want := map[string][4]float64{
		"CPU":                   {28.69, 0, 15.32},
		"RPC0[127.0.0.1:50551]": {28.72, 6.38, 29.93},
		"RPC0[127.0.0.1:50552]": {107.68, 17.53, 19.79},
	}
	for _, d := range devices {
		w, ok := want[d.Device]
		if !ok || d.WeightsMiB != w[0] || d.KVMiB != w[1] || d.ComputeMiB != w[2] {
			t.Fatalf("%+v", d)
		}
		if strings.HasPrefix(d.Device, "RPC") != (d.Endpoint != "") {
			t.Fatalf("endpoint %+v", d)
		}
	}
	if devices[1].Endpoint != "127.0.0.1:50551" {
		t.Fatalf("%+v", devices[1])
	}
}

func TestParseDeviceList(t *testing.T) {
	cpu := "load_backend: loaded CPU backend\nAvailable devices:\n  (none)\n"
	if got := ParseDeviceList(cpu); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	gpu := "Available devices:\n  Vulkan0: NVIDIA GeForce RTX 3050 (6144 MiB, 5800 MiB free)\n  CUDA0: NVIDIA (6144 MiB)\n  RPC0: 127.0.0.1:1 (100 MiB, 90 MiB free)\n"
	if got := ParseDeviceList(gpu); strings.Join(got, ",") != "Vulkan0,CUDA0" {
		t.Fatalf("%v", got)
	}
	if got := ParseDeviceList("  Vulkan0: before the header\n"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestArgs(t *testing.T) {
	c := LaunchConfig{Endpoint: "http://127.0.0.1:8080", Context: 4096, GPULayers: 999, RPC: []string{"127.0.0.1:5000", "127.0.0.1:5001"},
		Devices: []string{"Vulkan0", "RPC0", "RPC1"}, TensorSplit: []float64{1, 2.5, 0}, Parallel: 4, CacheTypeK: "q8_0", CacheTypeV: "q4_0", NoKVOffload: true, FlashAttn: "on", Verbose: true}
	args, err := Args(c, "/m.gguf", "qwen")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	for _, want := range []string{"--parallel 4", "--rpc 127.0.0.1:5000,127.0.0.1:5001", "--device Vulkan0,RPC0,RPC1", "--tensor-split 1,2.5,0", "--cache-type-k q8_0 --cache-type-v q4_0", "--no-kv-offload", "--flash-attn on", "--log-verbosity 4", "--alias qwen", "--ctx-size 4096"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	plain, _ := Args(LaunchConfig{Endpoint: "http://127.0.0.1:8080", Context: 512}, "/m", "m")
	for _, flag := range []string{"--device", "--tensor-split", "--cache-type", "--flash-attn", "--no-kv-offload", "--rpc", "--log-verbosity"} {
		if strings.Contains(strings.Join(plain, " "), flag) {
			t.Fatalf("defaults must not pass %s", flag)
		}
	}
	bad := []LaunchConfig{
		{Endpoint: "http://127.0.0.1:8080", Context: 512, RPC: []string{"10.0.0.1:5"}},
		{Endpoint: "http://127.0.0.1:8080", Context: 512, Devices: []string{"RPC0;rm"}},
		{Endpoint: "http://127.0.0.1:8080", Context: 512, Devices: []string{"RPC0"}, TensorSplit: []float64{1, 2}},
		{Endpoint: "http://127.0.0.1:8080", Context: 512, Devices: []string{"RPC0", "RPC1"}, TensorSplit: []float64{0, 0}},
		{Endpoint: "http://127.0.0.1:8080", Context: 512, Devices: []string{"none"}, TensorSplit: []float64{1}},
		{Endpoint: "http://127.0.0.1:8080", Context: 512, CacheTypeK: "q2_k"},
		{Endpoint: "http://127.0.0.1:8080", Context: 512, FlashAttn: "yes"},
		{Endpoint: "http://127.0.0.1:8080", Context: 512, Parallel: 65},
		{Endpoint: "http://127.0.0.1", Context: 512},
	}
	for _, b := range bad {
		if _, err := Args(b, "/m", "m"); err == nil {
			t.Errorf("accepted %+v", b)
		}
	}
}
