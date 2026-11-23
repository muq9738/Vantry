package api

import "testing"

func TestThreadReferenceNamesOnlyTheSession(t *testing.T) {
	for _, test := range []struct {
		name   string
		thread Thread
		want   string
	}{
		{
			name:   "coordinator title drops its project",
			thread: Thread{ID: "th_chat", Title: "项目协调员 · demo-app", ProjectPath: "/Users/me/Develop/demo-app"},
			want:   "[项目协调员](vantry-thread://th_chat)",
		},
		{
			name:   "project comes from the workspace when no project path is stored",
			thread: Thread{ID: "th_ws", Title: "项目协调员 · Vantry", Workspace: &WorkspaceSpec{Kind: "directory", Path: "/Projects/Vantry"}},
			want:   "[项目协调员](vantry-thread://th_ws)",
		},
		{
			name:   "a title that merely mentions the project keeps all of it",
			thread: Thread{ID: "th_ref", Title: "demo-app 重构", ProjectPath: "/Users/me/Develop/demo-app"},
			want:   "[demo-app 重构](vantry-thread://th_ref)",
		},
		{
			name:   "a renamed title stays as written",
			thread: Thread{ID: "th_renamed", Title: "重构 API · 第二阶段", ProjectPath: "/Users/me/Develop/demo-app"},
			want:   "[重构 API · 第二阶段](vantry-thread://th_renamed)",
		},
	} {
		if got := ThreadReference(test.thread); got != test.want {
			t.Errorf("%s: ThreadReference = %q, want %q", test.name, got, test.want)
		}
	}
}
