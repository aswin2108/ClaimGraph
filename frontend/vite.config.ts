import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// The dev server proxies /api to the Go API. changeOrigin rewrites the Host header to the
// API's own address, which its DNS-rebinding guard requires (localhost / 127.0.0.1 only).
const proxy = { "/api": { target: process.env.API_URL ?? "http://127.0.0.1:8080", changeOrigin: true } };

export default defineConfig({
  plugins: [react()],
  server: { port: Number(process.env.PORT ?? 5173), strictPort: true, proxy },
  test: { environment: "jsdom", setupFiles: ["./src/test/setup.ts"], include: ["src/**/*.test.{ts,tsx}"], restoreMocks: true },
  preview: { port: Number(process.env.PORT ?? 4173), strictPort: true, proxy },
});
