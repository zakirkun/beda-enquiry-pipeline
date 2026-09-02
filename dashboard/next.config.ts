import type { NextConfig } from "next";

const config: NextConfig = {
  // Standalone output keeps the runtime image small (see dashboard/Dockerfile).
  output: "standalone",
  // Pin the trace root to this app. Without it Next walks up and can pick a
  // lockfile outside the project, which puts the wrong files in the standalone
  // bundle.
  outputFileTracingRoot: import.meta.dirname,
};

export default config;
