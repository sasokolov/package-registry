import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The console is served from the registry binary under /ui/, so assets are
// requested relative to that base. Everything is emitted with a content
// hash: the server can then cache assets immutably and still never serve a
// stale bundle after an upgrade.
export default defineConfig({
  base: "/ui/",
  plugins: [react()],
  build: {
    outDir: "dist",
    emptyOutDir: true,
    // Keep the bundle honest: a console that ships a megabyte of
    // JavaScript to show a table is a console nobody opens on a bad link.
    chunkSizeWarningLimit: 400,
    rollupOptions: {
      output: {
        manualChunks: {
          react: ["react", "react-dom", "react-router-dom"],
        },
      },
    },
  },
  server: {
    // `npm run dev` proxies the API to a locally running registry, so the
    // console can be developed against real data.
    proxy: {
      "/api": "http://localhost:8080",
    },
  },
});
