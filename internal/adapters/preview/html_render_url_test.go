package preview

import "testing"

func TestFileURLFromSlashPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{
			in:   "C:/Users/admin/AppData/Local/Temp/preview.html",
			want: "file:///C:/Users/admin/AppData/Local/Temp/preview.html",
		},
		{
			in:   "/tmp/Vantry 测试/preview.html",
			want: "file:///tmp/Vantry%20%E6%B5%8B%E8%AF%95/preview.html",
		},
	}
	for _, tc := range cases {
		if got := fileURLFromSlashPath(tc.in); got != tc.want {
			t.Fatalf("fileURLFromSlashPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
