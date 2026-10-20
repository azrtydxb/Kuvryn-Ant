// Copies the management API contract (JSON stored as api/openapi.yaml) into the
// site's public folder so the Scalar reference at /api/ can load it.
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const source = resolve(here, "../../api/openapi.yaml");
const target = resolve(here, "../public/api/openapi.json");
const spec = JSON.parse(readFileSync(source, "utf8"));
mkdirSync(dirname(target), { recursive: true });
writeFileSync(target, JSON.stringify(spec));
console.log(
  `openapi: ${spec.info.title} ${spec.info.version} -> public/api/openapi.json`,
);
