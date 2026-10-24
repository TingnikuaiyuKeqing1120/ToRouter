import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": "http://127.0.0.1:8787",
      "/v1": "http://127.0.0.1:8787",
      "/v1beta": "http://127.0.0.1:8787",
    },
  },
  build: {
    outDir: "dist",
  },
  test: {
    setupFiles: ['./src/testSetup.ts'],
    testTimeout: 15000,
    minWorkers: 1,
    maxWorkers: 4,
  },
});
