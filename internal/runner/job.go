package runner

// A runner started for a Job is told what to do in its environment: it
// claims under CADRE_RUNNER (the Job's name), from CADRE_SERVER, in
// CADRE_NAMESPACE, the tasks of CADRE_AGENT only, once, and exits.
const (
	EnvServer    = "CADRE_SERVER"
	EnvNamespace = "CADRE_NAMESPACE"
	EnvRunner    = "CADRE_RUNNER"
	EnvAgent     = "CADRE_AGENT"
)

// JobEnv is what a Job's environment tells its runner.
type JobEnv struct {
	Server, Namespace, Runner, Agent string
}

// ReadJobEnv reads it with getenv; ok is false outside a Job, when
// CADRE_RUNNER is not set.
func ReadJobEnv(getenv func(string) string) (env JobEnv, ok bool) {
	env = JobEnv{Server: getenv(EnvServer), Namespace: getenv(EnvNamespace), Runner: getenv(EnvRunner), Agent: getenv(EnvAgent)}
	return env, env.Runner != ""
}
