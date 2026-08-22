import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Vite dev server: proxies /api to the Go server on :8080 so the frontend
// can call the backend without CORS during development.
export default defineConfig({
	plugins: [react()],
	server: {
		port: 5173,
		proxy: {
			"/api": { target: "http://localhost:8080", changeOrigin: true },
			"/healthz": { target: "http://localhost:8080", changeOrigin: true },
		},
	},
});
