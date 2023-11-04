package node

import (
	"container/list"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/grussorusso/serverledge/internal/config"
	"github.com/grussorusso/serverledge/internal/container"
	"github.com/grussorusso/serverledge/internal/function"
)

type ContainerPool struct {
	busy  *list.List // circular list of ContainerID
	ready *list.List // circular list of warmContainer
}

type warmContainer struct {
	Expiration int64
	Function   string
	Runtime    string
	contID     container.ContainerID
}

type busyContainer struct {
	Function string
	Runtime  string
	contID   container.ContainerID
}

var NoWarmFoundErr = errors.New("no warm container is available")

// GetFunctionPool retrieves (or creates) the container pool for a function.
func GetFunctionPool(f *function.Function) *ContainerPool {
	if fp, ok := Resources.ContainerPools[f.Name]; ok {
		return fp
	}

	fp := newFunctionPool()
	Resources.ContainerPools[f.Name] = fp
	return fp
}

func ArePoolsEmptyInThisNode() bool {
	return len(Resources.ContainerPools) == 0
}

func (fp *ContainerPool) getWarmContainer(f *function.Function) (container.ContainerID, bool) {
	// TODO: picking most-recent / least-recent container might be better?
	elem := fp.ready.Front()
	if elem == nil {
		return "", false
	}

	if elem.Value.(warmContainer).Function != f.Name {
		return "no function", false
	}

	if elem.Value.(warmContainer).Runtime != f.Runtime {
		return "no runtime", false
	}

	fp.ready.Remove(elem)
	contID := elem.Value.(warmContainer).contID
	fp.putBusyContainer(contID, f)

	return contID, true
}

func (fp *ContainerPool) putBusyContainer(contID container.ContainerID, f *function.Function) {
	// log.Printf("storing in the busy pool the container %s for func '%s' with runtime '%s'\n", contID, f.Name, f.Runtime)
	fp.busy.PushBack(busyContainer{ // creating
		Function: f.Name,
		Runtime:  f.Runtime,
		contID:   contID,
	})
}

func (fp *ContainerPool) putReadyContainer(contID container.ContainerID, busyContainer busyContainer, expiration int64) {
	funcName := busyContainer.Function
	runtime := busyContainer.Runtime
	// fmt.Printf("storing in the ready pool warm container %s for func '%s'\n", contID, funcName)

	fp.ready.PushBack(warmContainer{ // creates warmContainer
		contID:     contID,
		Function:   funcName,
		Runtime:    runtime,
		Expiration: expiration,
	})
}

func newFunctionPool() *ContainerPool {
	fp := &ContainerPool{}
	fp.busy = list.New()
	fp.ready = list.New()

	return fp
}

// AcquireResources reserves the specified amount of cpu and memory if possible.
func AcquireResources(cpuDemand float64, memDemand int64, destroyContainersIfNeeded bool) bool {
	Resources.Lock()
	defer Resources.Unlock()
	return acquireResources(cpuDemand, memDemand, destroyContainersIfNeeded)
}

// acquireResources reserves the specified amount of cpu and memory if possible.
// The function is NOT thread-safe.
func acquireResources(cpuDemand float64, memDemand int64, destroyContainersIfNeeded bool) bool {
	if Resources.AvailableCPUs < cpuDemand {
		return false
	}
	if Resources.AvailableMemMB < memDemand {
		if !destroyContainersIfNeeded {
			return false
		}

		enoughMem, _ := dismissContainer(memDemand)
		if !enoughMem {
			return false
		}
	}

	Resources.AvailableCPUs -= cpuDemand
	Resources.AvailableMemMB -= memDemand

	return true
}

// releaseResources releases the specified amount of cpu and memory.
// The function is NOT thread-safe.
func releaseResources(cpuDemand float64, memDemand int64) {
	Resources.AvailableCPUs += cpuDemand
	Resources.AvailableMemMB += memDemand
}

// AcquireWarmContainer acquires a warm container for a given function (if any).
// A warm container is in running/paused state and has already been initialized
// with the function code.
// The acquired container is already in the busy pool.
// The function returns an error if either:
// (i) the warm container does not exist
// (ii) there are not enough resources to start the container
func AcquireWarmContainer(f *function.Function) (container.ContainerID, error) {
	Resources.Lock()
	defer Resources.Unlock()

	fp := GetFunctionPool(f)
	// fmt.Printf("ready containers: %+v\nbusy containers: %+v\n", fp.ready.Len(), fp.busy.Len())
	contID, found := fp.getWarmContainer(f)
	if !found {
		if contID == "no function" {
			fmt.Printf("the container exists, but doesn't have the function %s\n", f.Name)
			return "", NoWarmFoundErr
		}
		if contID == "no runtime" {
			fmt.Printf("the container exists, but doesn't have the correct runtime (%s) for function %s\n", f.Runtime, f.Name)
			return "", NoWarmFoundErr
		}
		return "", NoWarmFoundErr
	}

	if !acquireResources(f.CPUDemand, 0, false) {
		//log.Printf("Not enough CPU to start a warm container for %s", f)
		return "", OutOfResourcesErr
	}

	//log.Printf("Acquired resources for warm container. Now: %v", Resources)
	return contID, nil
}

// ReleaseContainer puts a container in the ready pool for a function.
func ReleaseContainer(contID container.ContainerID, f *function.Function) {
	// setup Expiration as time duration from now
	d := time.Duration(config.GetInt(config.CONTAINER_EXPIRATION_TIME, 600)) * time.Second
	expTime := time.Now().Add(d).UnixNano()

	Resources.Lock()
	defer Resources.Unlock()
	fp := GetFunctionPool(f)
	// we must update the busy list by removing this element
	var deleted interface{}
	elem := fp.busy.Front()
	for ok := elem != nil; ok; ok = elem != nil {
		if elem.Value.(busyContainer).contID == contID {
			deleted = fp.busy.Remove(elem) // delete the element from the busy list
			break
		}
		elem = elem.Next()
	}
	if deleted != nil {
		fp.putReadyContainer(contID, deleted.(busyContainer), expTime) // FIXME: here there is a nil pointer dereference with high number of users
	}

	releaseResources(f.CPUDemand, 0)
}

// NewContainer creates and starts a new container for the given function.
// The container can be directly used to schedule a request, as it is already
// in the busy pool.
func NewContainer(fun *function.Function) (container.ContainerID, error) {
	Resources.Lock()
	if !acquireResources(fun.CPUDemand, fun.MemoryMB, true) {
		//log.Printf("Not enough resources for the new container.\n")
		Resources.Unlock()
		return "", OutOfResourcesErr
	}

	//log.Printf("Acquired resources for new container. Now: %v", Resources)
	Resources.Unlock()

	return NewContainerWithAcquiredResources(fun)
}

// NewContainerWithAcquiredResources spawns a new container for the given
// function, assuming that the required CPU and memory resources have been
// already been acquired.
func NewContainerWithAcquiredResources(fun *function.Function) (container.ContainerID, error) {
	var image string
	if fun.Runtime == container.CUSTOM_RUNTIME {
		image = fun.CustomImage
	} else {
		runtime, ok := container.RuntimeToInfo[fun.Runtime]
		if !ok {
			log.Printf("Unknown runtime: %s", fun.Runtime)
			return "", fmt.Errorf("Invalid runtime: %s", fun.Runtime)
		}
		image = runtime.Image
	}

	contID, err := container.NewContainer(image, fun.TarFunctionCode, &container.ContainerOptions{
		MemoryMB: fun.MemoryMB,
		CPUQuota: fun.CPUDemand,
	})

	if err != nil {
		log.Printf("Failed container creation: %v", err)
	}

	Resources.Lock()
	defer Resources.Unlock()
	if err != nil {
		releaseResources(fun.CPUDemand, fun.MemoryMB)
		return "", err
	}

	fp := GetFunctionPool(fun)
	fp.putBusyContainer(contID, fun) // We immediately mark it as busy

	return contID, nil
}

type itemToDismiss struct {
	contID container.ContainerID
	pool   *ContainerPool
	elem   *list.Element
	memory int64
}

// dismissContainer ... this function is used to get free memory used for a new container
// 2-phases: first, we find ready container and collect them as a slice, second (cleanup phase) we delete the container only and only if
// the sum of their memory is >= requiredMemoryMB is
func dismissContainer(requiredMemoryMB int64) (bool, error) {
	var cleanedMB int64 = 0
	var containerToDismiss []itemToDismiss
	res := false

	//first phase, research
	for _, funPool := range Resources.ContainerPools {
		if funPool.ready.Len() > 0 {
			// every container into the funPool has the same memory (same function)
			//so it is not important which one you destroy
			elem := funPool.ready.Front()
			contID := elem.Value.(warmContainer).contID
			// container in the same pool need same memory
			memory, _ := container.GetMemoryMB(contID)
			for ok := true; ok; ok = elem != nil {
				containerToDismiss = append(containerToDismiss,
					itemToDismiss{contID: contID, pool: funPool, elem: elem, memory: memory})
				cleanedMB += memory
				if cleanedMB >= requiredMemoryMB {
					goto cleanup
				}
				//go on to the next one
				elem = elem.Next()
			}
		}
	}

cleanup: // second phase, cleanup
	// memory check
	if cleanedMB >= requiredMemoryMB {
		for _, item := range containerToDismiss {
			item.pool.ready.Remove(item.elem)     // remove the container from the funPool
			err := container.Destroy(item.contID) // destroy the container
			if err != nil {
				res = false
				return res, nil
			}
			Resources.AvailableMemMB += item.memory
		}

		res = true
	}
	return res, nil
}

// DeleteExpiredContainer is called by the container cleaner
// Deletes expired warm container
func DeleteExpiredContainer() {
	now := time.Now().UnixNano()

	Resources.Lock()
	defer Resources.Unlock()

	for _, pool := range Resources.ContainerPools {
		elem := pool.ready.Front()
		for ok := elem != nil; ok; ok = elem != nil {
			warmed := elem.Value.(warmContainer)
			if now > warmed.Expiration {
				temp := elem
				elem = elem.Next()
				//log.Printf("cleaner: Removing container %s\n", warmed.contID)
				pool.ready.Remove(temp) // remove the expired element

				memory, _ := container.GetMemoryMB(warmed.contID)
				releaseResources(0, memory)
				container.Destroy(warmed.contID)
				//log.Printf("Released resources. Now: %v", Resources)
			} else {
				elem = elem.Next()
			}
		}
	}

}

// ShutdownWarmContainersFor destroys warm containers of a given function
// Actual termination happens asynchronously.
func ShutdownWarmContainersFor(f *function.Function) {
	Resources.Lock()
	defer Resources.Unlock()

	fp, ok := Resources.ContainerPools[f.Name]
	if !ok {
		return
	}

	containersToDelete := make([]container.ContainerID, 0)

	elem := fp.ready.Front()
	for ok := elem != nil; ok; ok = elem != nil {
		warmed := elem.Value.(warmContainer)
		temp := elem
		elem = elem.Next()
		log.Printf("Removing container with ID %s\n", warmed.contID)
		fp.ready.Remove(temp)

		memory, _ := container.GetMemoryMB(warmed.contID)
		Resources.AvailableMemMB += memory
		containersToDelete = append(containersToDelete, warmed.contID)
	}

	go func(contIDs []container.ContainerID) {
		for _, contID := range contIDs {
			// No need to update available resources here
			if err := container.Destroy(contID); err != nil {
				log.Printf("An error occurred while deleting %s: %v", contID, err)
			} else {
				log.Printf("Deleted %s", contID)
			}
		}
	}(containersToDelete)
}

// ShutdownAllContainers destroys all container (usually on termination)
func ShutdownAllContainers() {
	Resources.Lock()
	defer Resources.Unlock()

	for fun, pool := range Resources.ContainerPools {
		elem := pool.ready.Front()
		for ok := elem != nil; ok; ok = elem != nil {
			warmed := elem.Value.(warmContainer)
			temp := elem
			elem = elem.Next()
			log.Printf("Removing container with ID %s\n", warmed.contID)
			pool.ready.Remove(temp)

			memory, _ := container.GetMemoryMB(warmed.contID)
			container.Destroy(warmed.contID)
			Resources.AvailableMemMB += memory
		}

		function, _ := function.GetFunction(fun)

		elem = pool.busy.Front()
		for ok := elem != nil; ok; ok = elem != nil {
			contID := elem.Value.(busyContainer).contID
			temp := elem
			elem = elem.Next()
			log.Printf("Removing container with ID %s\n", contID)
			pool.ready.Remove(temp)

			memory, errMem := container.GetMemoryMB(contID)
			if errMem != nil {
				fmt.Printf("failed to get memory from container %s before destroying it: %v", contID, errMem)
				continue
			}
			err := container.Destroy(contID)
			if err != nil {
				fmt.Printf("failed to destroy container %s: %v\n", contID, err)
				continue
			}
			Resources.AvailableMemMB += memory
			Resources.AvailableCPUs += function.CPUDemand
		}
	}
}

// WarmStatus foreach function returns the corresponding number of warm container available
func WarmStatus() map[string]int {
	Resources.RLock()
	defer Resources.RUnlock()
	warmPool := make(map[string]int)
	for funcName, pool := range Resources.ContainerPools {
		warmPool[funcName] = pool.ready.Len()
	}

	return warmPool
}
