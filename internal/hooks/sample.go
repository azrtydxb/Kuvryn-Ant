package hooks

import (
	"encoding/json"
	"strings"
)

// SamplePayload builds the body and headers the console's "Send test" posts for
// an action. Upstream notifications use a representative payload of the chosen
// provider; templated actions get a document that contains exactly the paths
// their templates read, so the test exercises the real rendering; the others
// get a small ant.test document.
func SamplePayload(a Action) (body []byte, headers map[string]string) {
	headers = map[string]string{"Content-Type": "application/json"}
	switch a.Type {
	case ActionUpstream:
		p := a.Provider
		if p == "" || p == ProviderAuto {
			p = ProviderGitHub
		}
		return sampleUpstream(p, headers)
	case ActionPrewarm, ActionPurgeSource, ActionCleanCache:
		root := map[string]any{}
		for _, tpl := range []string{a.Name, a.Version, a.Coordinate, a.Prefix} {
			for _, path := range TemplatePaths(tpl) {
				setPath(root, path, sampleValue(path[len(path)-1]))
			}
		}
		if len(root) == 0 {
			root["event"] = "ant.test"
		}
		b, _ := json.Marshal(root)
		return b, headers
	}
	b, _ := json.Marshal(map[string]any{"event": "ant.test", "message": "Sample payload sent from the Ant console."})
	return b, headers
}

func sampleValue(key string) string {
	k := strings.ToLower(key)
	switch {
	case strings.Contains(k, "tag") || strings.Contains(k, "version") || k == "ref":
		return "1.2.3"
	case strings.Contains(k, "name") || strings.Contains(k, "repo") || strings.Contains(k, "image") || strings.Contains(k, "package"):
		return "sample-package"
	}
	return "sample"
}

func setPath(root map[string]any, path []string, value string) {
	cur := root
	for i, k := range path {
		if i == len(path)-1 {
			cur[k] = value
			return
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
}

func sampleUpstream(provider string, headers map[string]string) ([]byte, map[string]string) {
	var doc any
	switch provider {
	case ProviderDockerHub:
		doc = map[string]any{"callback_url": "https://registry.hub.docker.com/u/example/sample/hook/sample/", "push_data": map[string]any{"pushed_at": 1700000000, "pusher": "example", "tag": "latest"}, "repository": map[string]any{"name": "sample", "namespace": "example", "repo_name": "example/sample", "status": "Active"}}
	case ProviderGitLab:
		headers["X-Gitlab-Event"] = "Tag Push Hook"
		doc = map[string]any{"object_kind": "tag_push", "ref": "refs/tags/v1.2.3", "project": map[string]any{"path_with_namespace": "example/sample", "web_url": "https://gitlab.com/example/sample"}}
	case ProviderNPM:
		doc = map[string]any{"event": "package:publish", "name": "@example/sample", "type": "package", "version": "1.2.3"}
	case ProviderArtifactory:
		doc = map[string]any{"domain": "artifact", "event_type": "deployed", "data": map[string]any{"repo_key": "libs-release-local", "path": "org/example/sample/1.2.3/sample-1.2.3.jar", "name": "sample-1.2.3.jar"}}
	case ProviderNexus:
		headers["X-Nexus-Webhook-ID"] = "rm:repository:component"
		doc = map[string]any{"action": "CREATED", "repositoryName": "maven-releases", "component": map[string]any{"format": "maven2", "group": "org.example", "name": "sample", "version": "1.2.3"}}
	default:
		headers["X-GitHub-Event"] = "release"
		doc = map[string]any{"action": "published", "release": map[string]any{"tag_name": "v1.2.3"}, "repository": map[string]any{"full_name": "example/sample"}}
	}
	b, _ := json.Marshal(doc)
	return b, headers
}
