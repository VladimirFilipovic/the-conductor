import type { NextConfig } from "next";
import path from "path";

const nextConfig: NextConfig = {
  // Standalone bundles a minimal server + traced node_modules into
  // .next/standalone, so the runtime image needs no dev deps and no `next` CLI.
  output: "standalone",
  // Pin the trace root to this app so an unrelated parent lockfile can't drag
  // the standalone bundle up into a wrong directory (breaks the Docker copy).
  outputFileTracingRoot: path.join(process.cwd()),
  // The build image has no ESLint config wired; type-checking still runs and is
  // the guard we actually care about here.
  eslint: { ignoreDuringBuilds: true },
};

export default nextConfig;
