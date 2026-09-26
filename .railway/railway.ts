import {
  defineRailway,
  github,
  group,
  postgres,
  project,
  service,
} from "railway/iac";

const REPO = "VladimirFilipovic/the-conductor";
const BRANCH = "main";

export default defineRailway(() => {
  const db = postgres("Postgres");

  const engine = service("engine", {
    source: github(REPO, { branch: BRANCH }),
    build: { builder: "DOCKERFILE", dockerfilePath: "Dockerfile.engine" },
    start: "/usr/local/bin/engine-entrypoint.sh",
    healthcheck: "/healthz",
    healthcheckTimeout: 120,
    // Only the engine runs goose, so a second replica would race the migration.
    replicas: 1,
    deploy: { restartPolicyType: "ON_FAILURE", restartPolicyMaxRetries: 10 },
    env: {
      CONDUCTOR_DATABASE_URL: db.env.DATABASE_URL,
      CONDUCTOR_LOGS_ADDR: ":7090",
      LOG_LEVEL: "DEBUG",
      // Railway probes the healthcheck on PORT; /healthz lives on the log server.
      PORT: "7090",
    },
  });

  const apiserver = service("apiserver", {
    source: github(REPO, { branch: BRANCH }),
    // Same image as the engine; only the start command differs.
    build: { builder: "DOCKERFILE", dockerfilePath: "Dockerfile.engine" },
    start: "/usr/local/bin/apiserver-entrypoint.sh",
    healthcheck: "/v1/healthz",
    // Outlasts the entrypoint's 120s wait for a seeded schema.
    healthcheckTimeout: 300,
    deploy: { restartPolicyType: "ON_FAILURE", restartPolicyMaxRetries: 10 },
    env: {
      CONDUCTOR_DATABASE_URL: db.env.DATABASE_URL,
      LOG_LEVEL: "DEBUG",
      // Two listeners (7080 HTTP, 7443 gRPC); point the healthcheck at HTTP.
      PORT: "7080",
    },
  });

  const agentsim = service("agentsim", {
    source: github(REPO, { branch: BRANCH }),
    build: { builder: "DOCKERFILE", dockerfilePath: "Dockerfile.agentsim" },
    deploy: { restartPolicyType: "ON_FAILURE", restartPolicyMaxRetries: 10 },
    env: {
      CONDUCTOR_AGENTAPI_ADDR: "apiserver.railway.internal:7443",
      CONDUCTOR_CONTROL_ADDR: ":7780",
    },
  });

  const chaosUi = service("chaos-ui", {
    // rootDirectory makes Railway detect chaos-ui/Dockerfile on its own.
    source: github(REPO, { branch: BRANCH, rootDirectory: "chaos-ui" }),
    deploy: { restartPolicyType: "ON_FAILURE", restartPolicyMaxRetries: 10 },
    env: {
      CONTROL_PLANE_URL: "http://apiserver.railway.internal:7080",
      AGENTSIM_URL: "http://agentsim.railway.internal:7780",
      ENGINE_LOG_URL: "http://engine.railway.internal:7090",
      PORT: "3000",
    },
  });

  return project("the-conductor", {
    resources: [
      group("Control plane", [db, engine, apiserver]),
      group("Chaos", [agentsim, chaosUi]),
    ],
  });
});
