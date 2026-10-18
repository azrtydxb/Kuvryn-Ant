import { defineConfig } from "vite";
export default defineConfig({
  base: "/console/",
  build: { outDir: "../internal/console/dist", emptyOutDir: true },
  server: {
    proxy: {
      "/api": { target: "http://127.0.0.1:8080", changeOrigin: false },
      "/auth": { target: "http://127.0.0.1:8080", changeOrigin: false },
    },
  },
});
