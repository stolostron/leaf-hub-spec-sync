package k8sworkerpool

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/go-logr/logr"
	"github.com/stolostron/leaf-hub-spec-sync/pkg/controller/rbac"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	envVarK8sClientsPoolSize  = "K8S_CLIENTS_POOL_SIZE"
	defaultK8sClientsPoolSize = 10
)

// AddK8sWorkerPool adds k8s workers pool to the manager and returns it.
func AddK8sWorkerPool(log logr.Logger, mgr ctrl.Manager) (*K8sWorkerPool, error) {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in cluster kubeconfig - %w", err)
	}

	// pool size is used for controller workers.
	// for impersonation workers we have additional workers, one per impersonated user.
	k8sWorkerPoolSize := readEnvVars(log)

	k8sWorkerPool := &K8sWorkerPool{
		log:                        log,
		kubeConfig:                 config,
		jobsQueue:                  make(chan *K8sJob, k8sWorkerPoolSize), // each worker can handle at most one job at a time
		poolSize:                   k8sWorkerPoolSize,
		impersonationManager:       rbac.NewImpersonationManager(config),
		impersonationWorkersQueues: make(map[string]chan *K8sJob),
		impersonationWorkersLock:   sync.Mutex{},
	}

	if err := mgr.Add(k8sWorkerPool); err != nil {
		return nil, fmt.Errorf("failed to initialize k8s workers pool - %w", err)
	}

	return k8sWorkerPool, nil
}

func readEnvVars(log logr.Logger) int {
	k8sClientsPoolSize, found := os.LookupEnv(envVarK8sClientsPoolSize)
	if !found {
		log.Info(fmt.Sprintf("env variable %s not found, using default value %d", envVarK8sClientsPoolSize,
			defaultK8sClientsPoolSize))

		return defaultK8sClientsPoolSize
	}

	value, err := strconv.Atoi(k8sClientsPoolSize)
	if err != nil || value < 1 {
		log.Info(fmt.Sprintf("env var %s invalid value: %s, using default value %d", envVarK8sClientsPoolSize,
			k8sClientsPoolSize, defaultK8sClientsPoolSize))

		return defaultK8sClientsPoolSize
	}

	return value
}

// K8sWorkerPool pool that creates all k8s workers and the assigns k8s jobs to available workers.
type K8sWorkerPool struct {
	ctx                        context.Context
	log                        logr.Logger
	kubeConfig                 *rest.Config
	jobsQueue                  chan *K8sJob
	poolSize                   int
	impersonationManager       *rbac.ImpersonationManager
	impersonationWorkersQueues map[string]chan *K8sJob
	impersonationWorkersLock   sync.Mutex
}

// Start function starts the k8s workers pool.
func (pool *K8sWorkerPool) Start(ctx context.Context) error {
	for i := 1; i <= pool.poolSize; i++ {
		worker, err := newK8sWorker(pool.log, i, pool.kubeConfig, pool.jobsQueue)
		if err != nil {
			return fmt.Errorf("failed to start k8s workers pool - %w", err)
		}

		worker.start(ctx)
	}

	pool.ctx = ctx

	<-ctx.Done() // blocking wait for stop event

	// context was cancelled, do cleanup
	close(pool.jobsQueue)

	return nil
}

// RunAsync inserts the K8sJob into the working queue.
func (pool *K8sWorkerPool) RunAsync(job *K8sJob) {
	userIdentity, err := pool.impersonationManager.GetUserIdentity(job.obj)
	if err != nil {
		pool.log.Error(err, "failed to get user identity from obj")
		return
	}
	// if it doesn't contain impersonation info, let the controller worker pool handle it.
	if userIdentity == rbac.NoUserIdentity {
		pool.jobsQueue <- job
		return
	}
	// otherwise, need to impersonate and use the specific worker to enforce permissions.
	pool.impersonationWorkersLock.Lock()

	if _, found := pool.impersonationWorkersQueues[userIdentity]; !found {
		if err := pool.createUserWorker(userIdentity); err != nil {
			pool.log.Error(err, "failed to create user worker", "user", userIdentity)
			return
		}
	}
	// push the job to the queue of the specific worker that uses the user identity
	workerQueue := pool.impersonationWorkersQueues[userIdentity]

	pool.impersonationWorkersLock.Unlock()
	workerQueue <- job // since this call might get blocking, first Unlock, then try to insert job into queue
}

func (pool *K8sWorkerPool) createUserWorker(userIdentity string) error {
	k8sClient, err := pool.impersonationManager.Impersonate(userIdentity)
	if err != nil {
		return fmt.Errorf("failed to impersonate - %w", err)
	}

	workerQueue := make(chan *K8sJob, pool.poolSize)
	worker := newK8sWorkerWithClient(pool.log.WithName(fmt.Sprintf("impersonation-%s", userIdentity)),
		1, k8sClient, workerQueue)
	worker.start(pool.ctx)
	pool.impersonationWorkersQueues[userIdentity] = workerQueue

	return nil
}
