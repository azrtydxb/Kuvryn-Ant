import type { SourceHealth } from "./generated";
import { Badge } from "./ds/bundle";

const tone = {
  healthy: "good",
  degraded: "warn",
  failing: "bad",
  unknown: "neutral",
  not_applicable: "neutral",
} as const;
export function HealthBadge({ h }: { h?: SourceHealth }) {
  if (!h || h.status === "not_applicable")
    return <span className="muted">—</span>;
  return (
    <Badge tone={tone[h.status]} dot>
      {h.status === "healthy"
        ? "Healthy"
        : h.status === "unknown"
          ? "No data yet"
          : h.status === "degraded"
            ? "Degraded"
            : "Failing"}
      {h.p95_latency_ms != null &&
        h.status !== "healthy" &&
        ` · ${h.p95_latency_ms} ms p95`}
    </Badge>
  );
}
