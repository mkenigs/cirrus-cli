package worker

import (
	"context"
	"errors"
	"github.com/cirruslabs/cirrus-ci-agent/api"
	"github.com/cirruslabs/cirrus-cli/internal/executor/instance/persistentworker"
	"github.com/cirruslabs/cirrus-cli/internal/executor/instance/runconfig"
	"google.golang.org/grpc"
	"time"
)

const perCallTimeout = 15 * time.Second

func (worker *Worker) runTask(ctx context.Context, agentAwareTask *api.PollResponse_AgentAwareTask) {
	if _, ok := worker.tasks[agentAwareTask.TaskId]; ok {
		worker.logger.Warnf("attempted to run task %d which is already running", agentAwareTask.TaskId)
		return
	}

	taskCtx, cancel := context.WithCancel(ctx)
	worker.tasks[agentAwareTask.TaskId] = cancel

	taskIdentification := &api.TaskIdentification{
		TaskId: agentAwareTask.TaskId,
		Secret: agentAwareTask.ClientSecret,
	}

	inst, err := persistentworker.New(agentAwareTask.Isolation, worker.logger)
	if err != nil {
		worker.logger.Errorf("failed to create an instance for the task %d: %v", agentAwareTask.TaskId, err)
		_, _ = worker.rpcClient.TaskFailed(ctx, &api.TaskFailedRequest{
			TaskIdentification: taskIdentification,
			Message:            err.Error(),
		}, grpc.PerRPCCredentials(worker))
		return
	}

	go func() {
		defer func() {
			if err := inst.Close(); err != nil {
				worker.logger.Errorf("failed to close persistent worker instance for task %d: %v",
					agentAwareTask.TaskId, err)
			}

			worker.taskCompletions <- agentAwareTask.TaskId
		}()
		_, err = worker.rpcClient.TaskStarted(taskCtx, taskIdentification, grpc.PerRPCCredentials(worker))
		if err != nil {
			worker.logger.Errorf("failed to notify the server about the started task %d: %v",
				agentAwareTask.TaskId, err)
			return
		}

		config := runconfig.RunConfig{
			ProjectDir:   "",
			Endpoint:     worker.agentEndpoint,
			ServerSecret: agentAwareTask.ServerSecret,
			ClientSecret: agentAwareTask.ClientSecret,
			TaskID:       agentAwareTask.TaskId,
		}

		if err := config.SetAgentVersionWithoutDowngrade(agentAwareTask.AgentVersion); err != nil {
			worker.logger.Warnf("failed to set agent's version for task %d: %v", agentAwareTask.TaskId, err)
		}

		err := inst.Run(taskCtx, &config)

		if err != nil && !errors.Is(err, context.Canceled) {
			worker.logger.Errorf("failed to run task %d: %v", agentAwareTask.TaskId, err)

			boundedCtx, cancel := context.WithTimeout(context.Background(), perCallTimeout)
			defer cancel()
			_, err := worker.rpcClient.TaskFailed(boundedCtx, &api.TaskFailedRequest{
				TaskIdentification: taskIdentification,
				Message:            err.Error(),
			}, grpc.PerRPCCredentials(worker))
			if err != nil {
				worker.logger.Errorf("failed to notify the server about the failed task %d: %v",
					agentAwareTask.TaskId, err)
			}
		}

		boundedCtx, cancel := context.WithTimeout(context.Background(), perCallTimeout)
		defer cancel()
		_, err = worker.rpcClient.TaskStopped(boundedCtx, taskIdentification, grpc.PerRPCCredentials(worker))
		if err != nil {
			worker.logger.Errorf("failed to notify the server about the stopped task %d: %v",
				agentAwareTask.TaskId, err)
			return
		}
	}()

	worker.logger.Infof("started task %d", agentAwareTask.TaskId)
}

func (worker *Worker) stopTask(taskID int64) {
	if cancel, ok := worker.tasks[taskID]; ok {
		cancel()
	}

	worker.logger.Infof("sent cancellation signal to task %d", taskID)
}

func (worker *Worker) runningTasks() (result []int64) {
	for taskID := range worker.tasks {
		result = append(result, taskID)
	}

	return
}

func (worker *Worker) registerTaskCompletions() {
	for {
		select {
		case taskID := <-worker.taskCompletions:
			if cancel, ok := worker.tasks[taskID]; ok {
				cancel()
				delete(worker.tasks, taskID)
				worker.logger.Infof("task %d completed", taskID)
			} else {
				worker.logger.Warnf("spurious task %d completed", taskID)
			}
		default:
			return
		}
	}
}
