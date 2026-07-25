package main

import "testing"

func TestResolverDirectives(t *testing.T) {
	tests := []struct {
		name string
		pnp  string
		want string
	}{{
		// What the kernel actually writes for our `ip=` boot param.
		name: "kernel pnp output",
		pnp:  "#MANUAL\nnameserver 8.8.8.8\nnameserver 8.8.4.4\n",
		want: "nameserver 8.8.8.8\nnameserver 8.8.4.4\n",
	}, {
		name: "drops bootserver and comments",
		pnp:  "#PROTO: DHCP\nbootserver 172.16.0.1\nnameserver 185.12.64.1\ndomain example.com\n",
		want: "nameserver 185.12.64.1\ndomain example.com\n",
	}, {
		name: "keeps search and options",
		pnp:  "search a.example b.example\noptions ndots:2\nnameserver 1.1.1.1\n",
		want: "search a.example b.example\noptions ndots:2\nnameserver 1.1.1.1\n",
	}, {
		// The caller treats empty as an error rather than truncating a
		// working resolv.conf to nothing.
		name: "no directives yields empty",
		pnp:  "#MANUAL\nbootserver 172.16.0.1\n\n",
		want: "",
	}, {
		name: "empty input",
		pnp:  "",
		want: "",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolverDirectives([]byte(tc.pnp)); got != tc.want {
				t.Errorf("resolverDirectives(%q):\n got %q\nwant %q", tc.pnp, got, tc.want)
			}
		})
	}
}
