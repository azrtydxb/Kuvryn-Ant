// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { FeedSourceIcons, SourceIcon, UpstreamPicker } from "./SourceIcon";
import { emptyDraft, pickPreset, draftProblem } from "./UpstreamFields";
import { upstreams } from "./upstreams";

afterEach(cleanup);

describe("SourceIcon", () => {
  it("renders the brand logo for a known remote", () => {
    const { container } = render(
      <SourceIcon
        source={{
          kind: "remote",
          format: "oci",
          origin: "https://registry-1.docker.io",
        }}
      />,
    );
    const tile = container.querySelector(".uicon") as HTMLElement;
    expect(tile.dataset.brand).toBe("docker");
    expect(tile.querySelector("svg path")).not.toBeNull();
    expect(tile.getAttribute("aria-hidden")).toBe("true");
  });

  it("falls back to globe, box and sync glyphs", () => {
    const glyph = (s: Parameters<typeof SourceIcon>[0]["source"]) => {
      const { container, unmount } = render(<SourceIcon source={s} />);
      const g = (container.querySelector(".uicon") as HTMLElement).dataset
        .glyph;
      unmount();
      return g;
    };
    expect(
      glyph({
        kind: "remote",
        format: "oci",
        origin: "https://artifacts.example.com",
      }),
    ).toBe("globe");
    expect(glyph({ kind: "remote", format: "npm" })).toBe("globe");
    expect(glyph({ kind: "local", format: "oci" })).toBe("box");
    expect(glyph({ kind: "replica", format: "oci" })).toBe("refresh-cw");
  });

  it("exposes a label when asked", () => {
    render(
      <SourceIcon
        source={{ kind: "local", format: "raw" }}
        label="Local source"
      />,
    );
    expect(screen.getByRole("img", { name: "Local source" })).toBeTruthy();
  });
});

describe("FeedSourceIcons", () => {
  const remote = (origin: string, name: string) => ({
    kind: "remote" as const,
    format: "oci" as const,
    origin,
    name,
  });
  it("shows at most three remote icons and a +N counter", () => {
    const { container } = render(
      <FeedSourceIcons
        sources={[
          remote("https://registry-1.docker.io", "a"),
          remote("https://ghcr.io", "b"),
          remote("https://quay.io", "c"),
          remote("https://gcr.io", "d"),
          remote("https://public.ecr.aws", "e"),
          { kind: "local", format: "oci", name: "loc" },
        ]}
      />,
    );
    expect(container.querySelectorAll(".uicon").length).toBe(3);
    expect(screen.getByText("+2")).toBeTruthy();
    expect(screen.getByRole("img").getAttribute("aria-label")).toContain(
      "Docker Hub",
    );
  });
  it("renders nothing without remote sources, and a cache mark for cache feeds", () => {
    const none = render(
      <FeedSourceIcons
        sources={[{ kind: "local", format: "oci", name: "l" }]}
      />,
    );
    expect(none.container.innerHTML).toBe("");
    none.unmount();
    const cache = render(<FeedSourceIcons sources={[]} cache />);
    expect(
      (cache.container.querySelector(".uicon") as HTMLElement).dataset.glyph,
    ).toBe("zap");
  });
});

describe("UpstreamPicker", () => {
  it("lists labelled preset tiles for the format plus Custom upstream", () => {
    const onPick = vi.fn();
    render(<UpstreamPicker format="oci" selected="ghcr" onPick={onPick} />);
    const docker = screen.getByRole("button", { name: "Docker Hub" });
    expect(docker.tagName).toBe("BUTTON");
    expect(
      screen
        .getByRole("button", { name: "GitHub Container Registry" })
        .getAttribute("aria-pressed"),
    ).toBe("true");
    expect(screen.queryByRole("button", { name: "npm" })).toBeNull();
    fireEvent.click(docker);
    expect(onPick).toHaveBeenCalledWith(
      upstreams.find((u) => u.id === "dockerhub"),
    );
    fireEvent.click(screen.getByRole("button", { name: "Custom upstream" }));
    expect(onPick).toHaveBeenLastCalledWith(undefined);
  });
});

describe("pickPreset", () => {
  it("prefills name, origin and redirects from a preset", () => {
    const d = pickPreset(
      emptyDraft,
      upstreams.find((u) => u.id === "pypi"),
    );
    expect(d).toEqual({
      preset: "pypi",
      name: "pypi-remote",
      origin: "https://pypi.org",
      redirects: "https://files.pythonhosted.org",
    });
  });
  it("lets the user edit after picking, and clears preset values for Custom", () => {
    const picked = pickPreset(
      emptyDraft,
      upstreams.find((u) => u.id === "go-proxy"),
    );
    const edited = { ...picked, name: "my-go" };
    expect(edited.origin).toBe("https://proxy.golang.org");
    expect(pickPreset(edited, undefined)).toEqual({
      ...emptyDraft,
      preset: "custom",
    });
    const typed = { ...emptyDraft, name: "x", origin: "https://x.example" };
    expect(pickPreset(typed, undefined)).toEqual({
      ...typed,
      preset: "custom",
    });
  });
  it("validates the draft", () => {
    expect(draftProblem(emptyDraft)).toMatch(/Enter/);
    expect(draftProblem({ ...emptyDraft, origin: "http://x.example" })).toMatch(
      /https/,
    );
    expect(
      draftProblem({ ...emptyDraft, origin: "https://x.example" }),
    ).toBeUndefined();
  });
});
