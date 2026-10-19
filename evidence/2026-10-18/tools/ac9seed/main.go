// Command ac9seed seeds and verifies deterministic raw objects for the upgrade rehearsal.
//
//	ac9seed seed   <base-url> <credential.json> <manifest.json>   create feeds upg-raw/upg-oci and publish 30 objects
//	ac9seed verify <base-url> <credential.json> <manifest.json>   re-read every object and compare SHA-256
//	ac9seed put    <base-url> <credential.json> <path>            publish one small object, print the HTTP status
//	ac9seed get    <base-url> <credential.json> <path>            print the HTTP status of reading one object
//
// The credential file is the JSON printed by `ant bootstrap` (field "token") or an automation token ("secret").
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/azrtydxb/Kuvryn-Ant/bench/antlib"
)

func main() {
	if len(os.Args) < 5 {
		fmt.Fprintln(os.Stderr, "usage: ac9seed seed|verify|put|get <base> <cred.json> <arg>")
		os.Exit(2)
	}
	tok, err := antlib.TokenFor(os.Args[3])
	if err != nil {
		panic(err)
	}
	api := antlib.New(os.Args[2], tok)
	switch os.Args[1] {
	case "seed":
		api.MakeFeed("upg-raw", "raw")
		api.MakeFeed("upg-oci", "oci")
		man := map[string]string{}
		for i := 0; i < 30; i++ {
			p := fmt.Sprintf("seed/o%d.bin", i)
			d := antlib.Det("ac9:"+p, 100000+i*1000)
			if s, _, body, _ := api.Req("PUT", "/feeds/upg-raw/raw/"+p, nil, antlib.Opt{Raw: d, Headers: map[string]string{"Content-Type": "application/octet-stream"}}); s != 201 {
				fmt.Println("put", p, "->", s, string(body))
			}
			man[p] = antlib.SHA(d)
		}
		b, _ := json.MarshalIndent(man, "", " ")
		if err := os.WriteFile(os.Args[4], b, 0o644); err != nil {
			panic(err)
		}
		fmt.Println("seeded", len(man))
	case "verify":
		var man map[string]string
		b, _ := os.ReadFile(os.Args[4])
		if err := json.Unmarshal(b, &man); err != nil {
			panic(err)
		}
		bad := 0
		for p, want := range man {
			if s, _, d := api.RawGet("upg-raw", p, ""); s != 200 || antlib.SHA(d) != want {
				bad++
			}
		}
		fmt.Printf("checked %d, mismatches %d\n", len(man), bad)
	case "anon": // anon <base> <cred> <slug>: does anonymous_read=true let an unauthenticated client read a raw feed?
		slug := os.Args[4]
		feed, _ := api.MakeFeed(slug, "raw")
		defer func() {
			_, _, _, _ = api.Req("DELETE", "/api/v1/feeds/"+feed.Str("id"), nil, antlib.Opt{Rev: api.Rev("feeds", feed.Str("id"))})
		}()
		fmt.Println("publish:", api.RawPut(slug, "a.txt", []byte("hello")))
		antlib.Must(api.OK("PATCH", "/api/v1/feeds/"+feed.Str("id"), antlib.Obj{"anonymous_read": true}, api.Rev("feeds", feed.Str("id"))))
		fmt.Println("feed anonymous_read:", api.FeedBySlug(slug)["anonymous_read"])
		fmt.Println("GET with token:", func() int { s, _, _ := api.RawGet(slug, "a.txt", ""); return s }())
		s, _, _, _ := antlib.New(os.Args[2], "").Req("GET", "/feeds/"+slug+"/raw/a.txt", nil)
		fmt.Println("GET without credentials:", s)
	case "put":
		fmt.Println(api.RawPut("upg-raw", os.Args[4], []byte("post-upgrade write")))
	case "get":
		s, _, _ := api.RawGet("upg-raw", os.Args[4], "")
		fmt.Println(s)
	}
}
