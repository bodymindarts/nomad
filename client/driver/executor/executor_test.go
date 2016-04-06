package executor

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/nomad/client/allocdir"
	"github.com/hashicorp/nomad/client/driver/env"
	cstructs "github.com/hashicorp/nomad/client/driver/structs"
	"github.com/hashicorp/nomad/client/testutil"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	tu "github.com/hashicorp/nomad/testutil"
)

var (
	constraint = &structs.Resources{
		CPU:      250,
		MemoryMB: 256,
		Networks: []*structs.NetworkResource{
			&structs.NetworkResource{
				MBits:        50,
				DynamicPorts: []structs.Port{{Label: "http"}},
			},
		},
	}
)

func mockAllocDir(t *testing.T) (*structs.Task, *allocdir.AllocDir) {
	alloc := mock.Alloc()
	task := alloc.Job.TaskGroups[0].Tasks[0]

	allocDir := allocdir.NewAllocDir(filepath.Join(os.TempDir(), alloc.ID))
	if err := allocDir.Build([]*structs.Task{task}); err != nil {
		log.Panicf("allocDir.Build() failed: %v", err)
	}

	return task, allocDir
}

func testExecutorContext(t *testing.T) *ExecutorContext {
	taskEnv := env.NewTaskEnvironment(mock.Node())
	task, allocDir := mockAllocDir(t)
	ctx := &ExecutorContext{
		TaskEnv:  taskEnv,
		Task:     task,
		AllocDir: allocDir,
	}
	return ctx
}

func TestExecutor_Start_Invalid(t *testing.T) {
	invalid := "/bin/foobar"
	execCmd := ExecCommand{Cmd: invalid, Args: []string{"1"}}
	ctx := testExecutorContext(t)
	defer ctx.AllocDir.Destroy()
	executor := NewExecutor(log.New(os.Stdout, "", log.LstdFlags))
	_, err := executor.LaunchCmd(&execCmd, ctx)
	if err == nil {
		t.Fatalf("Expected error")
	}
}

func TestExecutor_Start_Wait_Failure_Code(t *testing.T) {
	execCmd := ExecCommand{Cmd: "/bin/sleep", Args: []string{"fail"}}
	ctx := testExecutorContext(t)
	defer ctx.AllocDir.Destroy()
	executor := NewExecutor(log.New(os.Stdout, "", log.LstdFlags))
	ps, _ := executor.LaunchCmd(&execCmd, ctx)
	if ps.Pid == 0 {
		t.Fatalf("expected process to start and have non zero pid")
	}
	ps, _ = executor.Wait()
	if ps.ExitCode < 1 {
		t.Fatalf("expected exit code to be non zero, actual: %v", ps.ExitCode)
	}
	if err := executor.Exit(); err != nil {
		t.Fatalf("error: %v", err)
	}
}

func TestExecutor_Start_Wait(t *testing.T) {
	execCmd := ExecCommand{Cmd: "/bin/echo", Args: []string{"hello world"}}
	ctx := testExecutorContext(t)
	defer ctx.AllocDir.Destroy()
	executor := NewExecutor(log.New(os.Stdout, "", log.LstdFlags))
	ps, err := executor.LaunchCmd(&execCmd, ctx)
	if err != nil {
		t.Fatalf("error in launching command: %v", err)
	}
	if ps.Pid == 0 {
		t.Fatalf("expected process to start and have non zero pid")
	}
	ps, err = executor.Wait()
	if err != nil {
		t.Fatalf("error in waiting for command: %v", err)
	}
	if err := executor.Exit(); err != nil {
		t.Fatalf("error: %v", err)
	}

	expected := "hello world"
	file := filepath.Join(ctx.AllocDir.LogDir(), "web.stdout.0")
	output, err := ioutil.ReadFile(file)
	if err != nil {
		t.Fatalf("Couldn't read file %v", file)
	}

	act := strings.TrimSpace(string(output))
	if act != expected {
		t.Fatalf("Command output incorrectly: want %v; got %v", expected, act)
	}
}

func TestExecutor_WaitExitSignal(t *testing.T) {
	execCmd := ExecCommand{Cmd: "/bin/sleep", Args: []string{"10000"}}
	ctx := testExecutorContext(t)
	defer ctx.AllocDir.Destroy()
	executor := NewExecutor(log.New(os.Stdout, "", log.LstdFlags))
	ps, err := executor.LaunchCmd(&execCmd, ctx)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	go func() {
		time.Sleep(1 * time.Second)
		proc, err := os.FindProcess(ps.Pid)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if err := proc.Signal(syscall.SIGKILL); err != nil {
			t.Fatalf("err: %v", err)
		}
	}()

	ps, err = executor.Wait()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ps.Signal != int(syscall.SIGKILL) {
		t.Fatalf("expected signal: %v, actual: %v", int(syscall.SIGKILL), ps.Signal)
	}
}

func TestExecutor_IsolationAndConstraints(t *testing.T) {
	testutil.ExecCompatible(t)

	execCmd := ExecCommand{Cmd: "/bin/echo", Args: []string{"hello world"}}
	ctx := testExecutorContext(t)
	defer ctx.AllocDir.Destroy()

	execCmd.FSIsolation = true
	execCmd.ResourceLimits = true
	execCmd.User = cstructs.DefaultUnpriviledgedUser

	executor := NewExecutor(log.New(os.Stdout, "", log.LstdFlags))
	ps, err := executor.LaunchCmd(&execCmd, ctx)
	if err != nil {
		t.Fatalf("error in launching command: %v", err)
	}
	if ps.Pid == 0 {
		t.Fatalf("expected process to start and have non zero pid")
	}
	_, err = executor.Wait()
	if err != nil {
		t.Fatalf("error in waiting for command: %v", err)
	}

	// Check if the resource contraints were applied
	memLimits := filepath.Join(ps.IsolationConfig.CgroupPaths["memory"], "memory.limit_in_bytes")
	data, err := ioutil.ReadFile(memLimits)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	expectedMemLim := strconv.Itoa(ctx.Task.Resources.MemoryMB * 1024 * 1024)
	actualMemLim := strings.TrimSpace(string(data))
	if actualMemLim != expectedMemLim {
		t.Fatalf("actual mem limit: %v, expected: %v", string(data), expectedMemLim)
	}

	if err := executor.Exit(); err != nil {
		t.Fatalf("error: %v", err)
	}

	// Check if Nomad has actually removed the cgroups
	if _, err := os.Stat(memLimits); err == nil {
		t.Fatalf("file %v hasn't been removed", memLimits)
	}

	expected := "hello world"
	file := filepath.Join(ctx.AllocDir.LogDir(), "web.stdout.0")
	output, err := ioutil.ReadFile(file)
	if err != nil {
		t.Fatalf("Couldn't read file %v", file)
	}

	act := strings.TrimSpace(string(output))
	if act != expected {
		t.Fatalf("Command output incorrectly: want %v; got %v", expected, act)
	}
}

func TestExecutor_DestroyCgroup(t *testing.T) {
	testutil.ExecCompatible(t)

	execCmd := ExecCommand{Cmd: "/bin/bash", Args: []string{"-c", "/usr/bin/yes"}}
	ctx := testExecutorContext(t)
	ctx.Task.LogConfig.MaxFiles = 1
	ctx.Task.LogConfig.MaxFileSizeMB = 300
	defer ctx.AllocDir.Destroy()

	execCmd.FSIsolation = true
	execCmd.ResourceLimits = true
	execCmd.User = "nobody"

	executor := NewExecutor(log.New(os.Stdout, "", log.LstdFlags))
	ps, err := executor.LaunchCmd(&execCmd, ctx)
	if err != nil {
		t.Fatalf("error in launching command: %v", err)
	}
	if ps.Pid == 0 {
		t.Fatalf("expected process to start and have non zero pid")
	}
	time.Sleep(200 * time.Millisecond)
	if err := executor.Exit(); err != nil {
		t.Fatalf("err: %v", err)
	}

	file := filepath.Join(ctx.AllocDir.LogDir(), "web.stdout.0")
	finfo, err := os.Stat(file)
	if err != nil {
		t.Fatalf("error stating stdout file: %v", err)
	}
	time.Sleep(1 * time.Second)
	finfo1, err := os.Stat(file)
	if err != nil {
		t.Fatalf("error stating stdout file: %v", err)
	}
	if finfo.Size() != finfo1.Size() {
		t.Fatalf("Expected size: %v, actual: %v", finfo.Size(), finfo1.Size())
	}
}

func TestExecutor_Start_Kill(t *testing.T) {
	execCmd := ExecCommand{Cmd: "/bin/sleep", Args: []string{"10 && hello world"}}
	ctx := testExecutorContext(t)
	defer ctx.AllocDir.Destroy()
	executor := NewExecutor(log.New(os.Stdout, "", log.LstdFlags))
	ps, err := executor.LaunchCmd(&execCmd, ctx)
	if err != nil {
		t.Fatalf("error in launching command: %v", err)
	}
	if ps.Pid == 0 {
		t.Fatalf("expected process to start and have non zero pid")
	}
	ps, err = executor.Wait()
	if err != nil {
		t.Fatalf("error in waiting for command: %v", err)
	}
	if err := executor.Exit(); err != nil {
		t.Fatalf("error: %v", err)
	}

	file := filepath.Join(ctx.AllocDir.LogDir(), "web.stdout.0")
	time.Sleep(time.Duration(tu.TestMultiplier()*2) * time.Second)

	output, err := ioutil.ReadFile(file)
	if err != nil {
		t.Fatalf("Couldn't read file %v", file)
	}

	expected := ""
	act := strings.TrimSpace(string(output))
	if act != expected {
		t.Fatalf("Command output incorrectly: want %v; got %v", expected, act)
	}
}

func TestExecutor_ResourceStats(t *testing.T) {
	testutil.ExecCompatible(t)

	execCmd := ExecCommand{Cmd: "/bin/bash", Args: []string{"-c", "/usr/bin/yes"}}
	ctx := testExecutorContext(t)
	ctx.Task.LogConfig.MaxFiles = 1
	ctx.Task.LogConfig.MaxFileSizeMB = 300
	defer ctx.AllocDir.Destroy()

	execCmd.FSIsolation = true
	execCmd.ResourceLimits = true
	execCmd.User = "nobody"

	executor := NewExecutor(log.New(os.Stdout, "", log.LstdFlags))
	ps, err := executor.LaunchCmd(&execCmd, ctx)
	if err != nil {
		t.Fatalf("error in launching command: %v", err)
	}
	if ps.Pid == 0 {
		t.Fatalf("expected process to start and have non zero pid")
	}
	time.Sleep(200 * time.Millisecond)
	stats, serr := executor.ResourceStats()
	if err := executor.Exit(); err != nil {
		t.Fatalf("err: %v", err)
	}

	if serr != nil {
		t.Fatalf("err: %v", serr)
	}

	fmt.Printf("DIPTANU STATS %v", stats)
}

func TestExecutor_MakeExecutable(t *testing.T) {
	// Create a temp file
	f, err := ioutil.TempFile("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	defer os.Remove(f.Name())

	// Set its permissions to be non-executable
	f.Chmod(os.FileMode(0610))

	// Make a fake exececutor
	ctx := testExecutorContext(t)
	defer ctx.AllocDir.Destroy()
	executor := NewExecutor(log.New(os.Stdout, "", log.LstdFlags))

	err = executor.(*UniversalExecutor).makeExecutable(f.Name())
	if err != nil {
		t.Fatalf("makeExecutable() failed: %v", err)
	}

	// Check the permissions
	stat, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat() failed: %v", err)
	}

	act := stat.Mode().Perm()
	exp := os.FileMode(0755)
	if act != exp {
		t.Fatalf("expected permissions %v; got %v", err)
	}
}

func TestExecutorInterpolateServices(t *testing.T) {
	task := mock.Job().TaskGroups[0].Tasks[0]
	// Make a fake exececutor
	ctx := testExecutorContext(t)
	defer ctx.AllocDir.Destroy()
	executor := NewExecutor(log.New(os.Stdout, "", log.LstdFlags))

	executor.(*UniversalExecutor).ctx = ctx
	executor.(*UniversalExecutor).interpolateServices(task)
	expectedTags := []string{"pci:true", "datacenter:dc1"}
	if !reflect.DeepEqual(task.Services[0].Tags, expectedTags) {
		t.Fatalf("expected: %v, actual: %v", expectedTags, task.Services[0].Tags)
	}

	expectedCheckCmd := "/usr/local/check-table-mysql"
	expectedCheckArgs := []string{"5.6"}
	if !reflect.DeepEqual(task.Services[0].Checks[0].Command, expectedCheckCmd) {
		t.Fatalf("expected: %v, actual: %v", expectedCheckCmd, task.Services[0].Checks[0].Command)
	}

	if !reflect.DeepEqual(task.Services[0].Checks[0].Args, expectedCheckArgs) {
		t.Fatalf("expected: %v, actual: %v", expectedCheckArgs, task.Services[0].Checks[0].Args)
	}
}
