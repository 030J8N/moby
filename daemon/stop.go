package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"time"

	"github.com/docker/docker/container"
	"github.com/docker/docker/errdefs"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// ContainerStop looks for the given container and stops it.
// In case the container fails to stop gracefully within a time duration
// specified by the timeout argument, in seconds, it is forcefully
// terminated (killed).
//
// If the timeout is nil, the container's StopTimeout value is used, if set,
// otherwise the engine default. A negative timeout value can be specified,
// meaning no timeout, i.e. no forceful termination is performed.
func (daemon *Daemon) ContainerStop(name string, timeout *int) error {
	ctr, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}
	if !ctr.IsRunning() {
		return containerNotModifiedError{}
	}
	err = daemon.containerStop(ctr, timeout)
	if err != nil {
		return errdefs.System(errors.Wrapf(err, "cannot stop container: %s", name))
	}
	return nil
}

// containerStop sends a stop signal, waits, sends a kill signal.
func (daemon *Daemon) containerStop(ctr *container.Container, seconds *int) error {
	// TODO propagate a context down to this function
	ctx := context.TODO()
	if !ctr.IsRunning() {
		return nil
	}

	var (
		stopSignal  = ctr.StopSignal()
		stopTimeout = ctr.StopTimeout()
	)
	if seconds != nil {
		stopTimeout = *seconds
	}

	var wait time.Duration
	if stopTimeout >= 0 {
		wait = time.Duration(stopTimeout) * time.Second
	}
	success := func() error {
		daemon.LogContainerEvent(ctr, "stop")
		return nil
	}

	// 1. Send a stop signal
	err := daemon.killPossiblyDeadProcess(ctr, stopSignal)
	if err != nil {
		wait = 2 * time.Second
	}

	var subCtx context.Context
	var cancel context.CancelFunc
	if stopTimeout >= 0 {
		subCtx, cancel = context.WithTimeout(ctx, wait)
	} else {
		subCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	if status := <-ctr.Wait(subCtx, container.WaitConditionNotRunning); status.Err() == nil {
		// container did exit, so ignore any previous errors and return
		return success()
	}

	if err != nil {
		// the container has still not exited, and the kill function errored, so log the error here:
		logrus.WithError(err).WithField("container", ctr.ID).Errorf("Error sending stop (signal %d) to container", stopSignal)
	}
	if stopTimeout < 0 {
		// if the client requested that we never kill / wait forever, but container.Wait was still
		// interrupted (parent context cancelled, for example), we should propagate the signal failure
		return err
	}

	logrus.WithField("container", ctr.ID).Infof("Container failed to exit within %s of signal %d - using the force", wait, stopSignal)
	// Stop either failed or container didnt exit, so fallback to kill.
	if err := daemon.Kill(ctr); err != nil {
		// got a kill error, but give container 2 more seconds to exit just in case
		subCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if status := <-ctr.Wait(subCtx, container.WaitConditionNotRunning); status.Err() == nil {
			// container did exit, so ignore error and return
			return success()
		}
		logrus.WithError(err).WithField("container", ctr.ID).Error("Error killing the container")
		return err
	}

	return success()
}
