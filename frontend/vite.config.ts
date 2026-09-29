import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Dev server proxies API calls to the Go backend; the Go binary also
// embeds the built frontend for single-binary local operation.
export default defineConfig({
	plugins: [react()],
	build: {
		outDir: "../web/dist",
		emptyOutDir: true,
	},
	server: {
		port: 5173,
		proxy: {
			"/api": "http://localhost:8080",
		},
	},
});
