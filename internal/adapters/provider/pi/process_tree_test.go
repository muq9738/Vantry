package pi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/pkg/provider"
)

func TestDetachedNodeToolsAreStoppedBeforeWorkspaceReuse(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node is required to reproduce Pi's detached tool spawning")
	}
	for _, mode := range []string{"worker-kill", "explicit-stop"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			fake := filepath.Join(root, "node-pi")
			toolPIDPath, piPIDPath, escaped := filepath.Join(root, "tool-pid"), filepath.Join(root, "pi-pid"), filepath.Join(root, "escaped")
			body := "echo $$ > " + shellQuote(toolPIDPath) + "; sleep 30; echo escaped > " + shellQuote(escaped)
			script := fmt.Sprintf(`#!/usr/bin/env node
const fs=require('node:fs'); const {spawn}=require('node:child_process'); process.title='pi';
fs.writeFileSync(%q,String(process.pid));
let input='';
process.stdin.on('data',chunk=>{
  input+=chunk.toString();
  for(let i; (i=input.indexOf('\n'))>=0;) {
    const c=JSON.parse(input.slice(0,i)); input=input.slice(i+1);
    console.log(JSON.stringify({type:'response',id:c.id,command:c.type,success:true,data:{disposition:'started'}}));
    if(c.type==='prompt') {
      spawn('/bin/sh',['-c',%q],{detached:true,stdio:'ignore'});
      console.log(JSON.stringify({type:'message_update',assistantMessageEvent:{type:'text_delta',delta:'working'}}));
      setInterval(()=>{},1000);
    }
  }
});
`, piPIDPath, body)
			if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			instance, request := newTestProvider(t, root, fake)
			initial, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			err := instance.RunEvents(initial, request, func(provider.Event) error { return nil })
			cancel()
			if !errors.Is(err, provider.ErrDetached) {
				t.Fatalf("initial observation: %v", err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(toolPIDPath); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("detached tool did not start")
				}
				time.Sleep(20 * time.Millisecond)
			}
			toolPID, err := strconv.Atoi(strings.TrimSpace(readFile(t, toolPIDPath)))
			if err != nil {
				t.Fatal(err)
			}
			piPID, err := strconv.Atoi(strings.TrimSpace(readFile(t, piPIDPath)))
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Kill(-toolPID, syscall.SIGKILL)
			defer syscall.Kill(-piPID, syscall.SIGKILL)
			defer instance.Stop(context.Background(), request)

			releaseWriter := make(chan struct{})
			writerAcquired := make(chan error, 1)
			if mode == "worker-kill" {
				// A legitimate second writer is already queued on the shared cwd.
				// Old-attempt finalization must not wait for that new writer.
				defer close(releaseWriter)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
					defer cancel()
					lock, err := workspace.LockDirectory(ctx, instance.options.DefaultWorkspace.Path)
					writerAcquired <- err
					if err == nil {
						<-releaseWriter
						workspace.Unlock(lock)
					}
				}()
				var marker startedMarker
				if _, err := readJSONFile(filepath.Join(root, "jobs", request.AttemptID, "started.json"), &marker); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Kill(marker.PID, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
			} else if err := instance.Stop(context.Background(), request); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			err = instance.RunEvents(ctx, request, func(provider.Event) error { return nil })
			cancel()
			if mode == "worker-kill" {
				if err == nil || !strings.Contains(err.Error(), "will not be resent") {
					t.Fatalf("worker recovery: %v", err)
				}
				select {
				case err := <-writerAcquired:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("workspace was not released to the next writer")
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatalf("explicit stop: %v", err)
			}
			members, err := processGroups()
			if err != nil {
				t.Fatal(err)
			}
			for _, member := range members {
				if member.group == piPID || member.group == toolPID {
					t.Fatalf("detached process group survived: %+v", member)
				}
			}
			if _, err := os.Stat(escaped); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old tool continued writing after completion: %v", err)
			}
		})
	}
}
