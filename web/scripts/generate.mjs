import { readFileSync, writeFileSync } from "node:fs";
const spec = JSON.parse(
  readFileSync(new URL("../../api/openapi.yaml", import.meta.url), "utf8"),
);
function type(s) {
  if (s.$ref) return s.$ref.split("/").at(-1);
  if (s.enum) return s.enum.map((v) => JSON.stringify(v)).join(" | ");
  if (s.type === "array") return `Array<${type(s.items)}>`;
  if (s.type === "object")
    return `{ ${Object.entries(s.properties ?? {})
      .map(
        ([k, v]) =>
          `${JSON.stringify(k)}${s.required?.includes(k) ? "" : "?"}: ${type(v)}`,
      )
      .join("; ")} }`;
  if (s.type === "integer" || s.type === "number") return "number";
  if (["string", "boolean"].includes(s.type)) return s.type;
  throw Error("Unsupported schema: " + JSON.stringify(s));
}
let result =
  "// Generated from api/openapi.yaml by npm run generate. Do not edit.\n";
for (const [name, schema] of Object.entries(spec.components.schemas))
  result += `export type ${name} = ${type(schema)};\n`;
result += `export const formats = ${JSON.stringify(spec.components.schemas.Feed.properties.format.enum)} as const;\n`;
result += "export interface Operations {\n";
let routes = {};
for (const [path, entry] of Object.entries(spec.paths))
  for (const [method, op] of Object.entries(entry)) {
    if (!op.operationId) continue;
    const response = Object.entries(op.responses).find(([status]) =>
      status.startsWith("2"),
    )[1];
    const json = response.content?.["application/json"]?.schema;
    result += `${op.operationId}: { body: ${op.requestBody ? type(op.requestBody.content["application/json"].schema) : "undefined"}; response: ${json ? type(json) : "undefined"} };\n`;
    routes[op.operationId] = {
      path,
      method: method.toUpperCase(),
      idempotent: [...(entry.parameters ?? []), ...(op.parameters ?? [])].some(
        (p) => p.in === "header" && p.name === "Idempotency-Key",
      ),
    };
  }
result +=
  "}\nexport const routes = " +
  JSON.stringify(routes, null, 2) +
  " as const;\n";
const file = new URL("../src/generated.ts", import.meta.url);
if (process.argv.includes("--check")) {
  if (readFileSync(file, "utf8") !== result)
    throw Error("API types are stale; run npm run generate.");
} else writeFileSync(file, result);
