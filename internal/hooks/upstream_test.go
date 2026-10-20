package hooks

import (
	"net/http"
	"reflect"
	"testing"
)

func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

type parseCase struct {
	name     string
	provider string
	header   http.Header
	body     string
	want     Change
	detect   string
}

var parseCases = []parseCase{
	{name: "dockerhub push", provider: ProviderAuto, detect: ProviderDockerHub,
		body: `{"callback_url":"https://registry.hub.docker.com/u/acme/api/hook/abc/","push_data":{"pushed_at":1700000000,"pusher":"bob","tag":"1.4.0"},"repository":{"name":"api","namespace":"acme","repo_name":"acme/api","status":"Active"}}`,
		want: Change{Provider: ProviderDockerHub, Event: "push", Names: []string{"acme/api"}, Version: "1.4.0", Kind: "image", Origins: dockerHubHosts}},
	{name: "dockerhub official image", provider: ProviderDockerHub,
		body: `{"push_data":{"tag":"latest"},"repository":{"repo_name":"nginx"}}`,
		want: Change{Provider: ProviderDockerHub, Event: "push", Names: []string{"nginx", "library/nginx"}, Version: "latest", Kind: "image", Origins: dockerHubHosts}},
	{name: "github release", provider: ProviderAuto, detect: ProviderGitHub, header: hdr("X-GitHub-Event", "release"),
		body: `{"action":"published","release":{"tag_name":"v3.1.0","draft":false},"repository":{"full_name":"acme/tool"}}`,
		want: Change{Provider: ProviderGitHub, Event: "release", Names: []string{"acme/tool"}, Version: "v3.1.0", Kind: "package", Origins: githubHosts}},
	{name: "github registry_package container", provider: ProviderGitHub, header: hdr("X-GitHub-Event", "registry_package"),
		body: `{"action":"published","registry_package":{"name":"API","namespace":"Acme","package_type":"CONTAINER","package_version":{"version":"sha256:abc","container_metadata":{"tag":{"name":"v9"}}}}}`,
		want: Change{Provider: ProviderGitHub, Event: "registry_package", Names: []string{"acme/api"}, Version: "v9", Kind: "image", Origins: []string{"ghcr.io"}}},
	{name: "github package npm", provider: ProviderGitHub, header: hdr("X-GitHub-Event", "package"),
		body: `{"action":"published","package":{"name":"lib","namespace":"acme","package_type":"npm","package_version":{"version":"2.0.1"}}}`,
		want: Change{Provider: ProviderGitHub, Event: "package", Names: []string{"lib", "@acme/lib"}, Version: "2.0.1", Kind: "package", Origins: []string{"npm.pkg.github.com"}}},
	{name: "github ping ignored", provider: ProviderGitHub, header: hdr("X-GitHub-Event", "ping"), body: `{"zen":"Keep it logically awesome."}`,
		want: Change{Provider: ProviderGitHub, Event: "ping", Kind: "package", Ignored: "ping"}},
	{name: "github unsupported event", provider: ProviderGitHub, header: hdr("X-GitHub-Event", "star"), body: `{"action":"created"}`,
		want: Change{Provider: ProviderGitHub, Event: "star", Kind: "package", Ignored: "unsupported_event"}},
	{name: "gitlab tag push self-managed", provider: ProviderAuto, detect: ProviderGitLab, header: hdr("X-Gitlab-Event", "Tag Push Hook"),
		body: `{"object_kind":"tag_push","ref":"refs/tags/v0.9.0","project":{"path_with_namespace":"grp/proj","web_url":"https://git.example.com/grp/proj"}}`,
		want: Change{Provider: ProviderGitLab, Event: "Tag Push Hook", Names: []string{"grp/proj"}, Version: "v0.9.0", Kind: "package", Origins: []string{"git.example.com"}}},
	{name: "gitlab push on gitlab.com", provider: ProviderGitLab, header: hdr("X-Gitlab-Event", "Push Hook"),
		body: `{"object_kind":"push","ref":"refs/heads/main","project":{"path_with_namespace":"grp/proj","web_url":"https://gitlab.com/grp/proj"}}`,
		want: Change{Provider: ProviderGitLab, Event: "Push Hook", Names: []string{"grp/proj"}, Version: "main", Kind: "package", Origins: []string{"gitlab.com", "registry.gitlab.com"}}},
	{name: "gitlab release", provider: ProviderGitLab, header: hdr("X-Gitlab-Event", "Release Hook"),
		body: `{"object_kind":"release","action":"create","tag":"v7","project":{"path_with_namespace":"grp/proj","web_url":"https://gitlab.com/grp/proj"}}`,
		want: Change{Provider: ProviderGitLab, Event: "Release Hook", Names: []string{"grp/proj"}, Version: "v7", Kind: "package", Origins: []string{"gitlab.com", "registry.gitlab.com"}}},
	{name: "npm publish scoped", provider: ProviderAuto, detect: ProviderNPM,
		body: `{"event":"package:publish","name":"@acme/ui","type":"package","version":"4.5.6","hookOwner":{"username":"bob"}}`,
		want: Change{Provider: ProviderNPM, Event: "package:publish", Names: []string{"@acme/ui", "@acme%2fui"}, Version: "4.5.6", Kind: "package", Origins: npmHosts}},
	{name: "npm star ignored", provider: ProviderNPM, body: `{"event":"package:star","name":"left-pad"}`,
		want: Change{Provider: ProviderNPM, Event: "package:star", Kind: "package", Ignored: "unsupported_event"}},
	{name: "artifactory artifact deployed", provider: ProviderAuto, detect: ProviderArtifactory,
		body: `{"domain":"artifact","event_type":"deployed","data":{"repo_key":"libs-release-local","path":"org/acme/core/2.0/core-2.0.jar","name":"core-2.0.jar","sha256":"ab"}}`,
		want: Change{Provider: ProviderArtifactory, Event: "artifact:deployed", Names: []string{"org/acme/core", "org/acme/core/2.0", "core-2.0.jar"}, Version: "2.0", Kind: "package"}},
	{name: "artifactory docker pushed", provider: ProviderArtifactory,
		body: `{"domain":"docker","event_type":"pushed","data":{"repo_key":"docker-local","path":"alpine/3.19","image_name":"alpine","tag":"3.19"}}`,
		want: Change{Provider: ProviderArtifactory, Event: "docker:pushed", Names: []string{"alpine"}, Version: "3.19", Kind: "image"}},
	{name: "nexus component created", provider: ProviderAuto, detect: ProviderNexus, header: hdr("X-Nexus-Webhook-ID", "rm:repository:component"),
		body: `{"action":"CREATED","repositoryName":"maven-releases","component":{"id":"x","componentId":"y","format":"maven2","name":"core","group":"org.acme","version":"1.0"}}`,
		want: Change{Provider: ProviderNexus, Event: "rm:repository:component:CREATED", Names: []string{"org.acme:core", "org/acme/core", "org.acme/core", "core"}, Version: "1.0", Kind: "package"}},
	{name: "nexus asset without header detected by shape", provider: ProviderAuto, detect: ProviderNexus,
		body: `{"action":"CREATED","repositoryName":"npm-hosted","asset":{"id":"a","assetId":"b","format":"npm","name":"lib/-/lib-1.0.0.tgz"}}`,
		want: Change{Provider: ProviderNexus, Event: ":CREATED", Names: []string{"lib/-", "lib/-/lib-1.0.0.tgz"}, Kind: "package"}},
}

func TestParseUpstreamProviderFixtures(t *testing.T) {
	for _, c := range parseCases {
		t.Run(c.name, func(t *testing.T) {
			h := c.header
			if h == nil {
				h = http.Header{}
			}
			got, err := ParseUpstream(c.provider, h, []byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got  %+v\nwant %+v", got, c.want)
			}
			if c.detect != "" && DetectProvider(h, decodePayload([]byte(c.body))) != c.detect {
				t.Fatalf("detected %q, want %q", DetectProvider(h, decodePayload([]byte(c.body))), c.detect)
			}
		})
	}
}

func TestParseUpstreamRejectsForeignPayloads(t *testing.T) {
	for name, body := range map[string]string{"not json": "hello", "array": `[1,2]`, "empty object": `{}`, "unrelated": `{"event":"deploy","service":"web"}`} {
		if _, err := ParseUpstream(ProviderAuto, http.Header{}, []byte(body)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	// A provider given explicitly rejects another provider's shape.
	if _, err := ParseUpstream(ProviderDockerHub, http.Header{}, []byte(`{"event":"package:publish","name":"x"}`)); err == nil {
		t.Fatal("dockerhub parser accepted an npm payload")
	}
	if _, err := ParseUpstream(ProviderGitLab, hdr("X-Gitlab-Event", "Push Hook"), []byte(`{"ref":"refs/heads/main"}`)); err == nil {
		t.Fatal("gitlab payload without a project accepted")
	}
}

func TestSampleUpstreamPayloadsParse(t *testing.T) {
	for _, p := range []string{ProviderDockerHub, ProviderGitHub, ProviderGitLab, ProviderNPM, ProviderArtifactory, ProviderNexus} {
		body, headers := SamplePayload(Action{Type: ActionUpstream, Provider: p})
		ch, err := ParseUpstream(p, headerOf(headers), body)
		if err != nil || ch.Ignored != "" || len(ch.Names) == 0 {
			t.Fatalf("%s sample: %+v %v", p, ch, err)
		}
	}
}

func TestMatchSourcesByOriginAndKind(t *testing.T) {
	all := []remoteSource{
		{id: "hub", format: "oci", origin: "https://registry-1.docker.io"},
		{id: "ghcr", format: "oci", origin: "https://ghcr.io"},
		{id: "npmjs", format: "npm", origin: "https://registry.npmjs.org/"},
		{id: "jfrog", format: "maven", origin: "https://artifacts.example.com/artifactory/libs"},
	}
	ids := func(in []remoteSource) []string {
		var out []string
		for _, r := range in {
			out = append(out, r.id)
		}
		return out
	}
	hub, _ := ParseUpstream(ProviderDockerHub, http.Header{}, []byte(`{"push_data":{"tag":"1"},"repository":{"repo_name":"a/b"}}`))
	if got := ids(matchSources(all, Action{}, hub)); !reflect.DeepEqual(got, []string{"hub"}) {
		t.Fatalf("dockerhub -> %v", got)
	}
	npm, _ := ParseUpstream(ProviderNPM, http.Header{}, []byte(`{"event":"package:publish","name":"x","version":"1"}`))
	if got := ids(matchSources(all, Action{}, npm)); !reflect.DeepEqual(got, []string{"npmjs"}) {
		t.Fatalf("npm -> %v", got)
	}
	// Artifactory names no server: only configured origins or sources match.
	art, _ := ParseUpstream(ProviderArtifactory, http.Header{}, []byte(`{"domain":"artifact","event_type":"deployed","data":{"path":"a/b/1/b-1.jar"}}`))
	if got := matchSources(all, Action{}, art); len(got) != 0 {
		t.Fatalf("artifactory matched without configuration: %v", got)
	}
	if got := ids(matchSources(all, Action{Origins: []string{"artifacts.example.com"}}, art)); !reflect.DeepEqual(got, []string{"jfrog"}) {
		t.Fatalf("origins -> %v", got)
	}
	if got := ids(matchSources(all, Action{SourceIDs: []string{"ghcr", "jfrog"}}, art)); !reflect.DeepEqual(got, []string{"ghcr", "jfrog"}) {
		t.Fatalf("explicit sources -> %v", got)
	}
	gh, _ := ParseUpstream(ProviderGitHub, hdr("X-GitHub-Event", "registry_package"), []byte(`{"registry_package":{"name":"app","namespace":"o","package_type":"CONTAINER","package_version":{"version":"1"}}}`))
	if got := ids(matchSources(all, Action{}, gh)); !reflect.DeepEqual(got, []string{"ghcr"}) {
		t.Fatalf("ghcr -> %v", got)
	}
}
