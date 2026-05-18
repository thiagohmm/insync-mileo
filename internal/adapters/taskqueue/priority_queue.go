package taskqueue

import (
	"container/heap"
	"fmt"
	"sync"
	"time"

	"github.com/thiagohmm/insync-clone/internal/domain"
)

// PriorityQueue implements a priority queue for sync tasks
type PriorityQueue struct {
	mu        sync.Mutex
	heap      *TaskHeap
	idCounter int64
}

// TaskHeap implements heap.Interface for SyncTask
type TaskHeap []*domain.SyncTask

func (h TaskHeap) Len() int { return len(h) }

func (h TaskHeap) Less(i, j int) bool {
	// Higher priority first (Critical > High > Normal > Low)
	if h[i].Priority != h[j].Priority {
		return h[i].Priority > h[j].Priority
	}
	// Earlier creation time first (FIFO within same priority)
	return h[i].CreatedAt.Before(h[j].CreatedAt)
}

func (h TaskHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *TaskHeap) Push(x interface{}) {
	task := x.(*domain.SyncTask)
	*h = append(*h, task)
}

func (h *TaskHeap) Pop() interface{} {
	old := *h
	n := len(old)
	task := old[n-1]
	old[n-1] = nil
	*h = old[0 : n-1]
	return task
}

// NewPriorityQueue creates a new priority queue
func NewPriorityQueue() *PriorityQueue {
	h := &TaskHeap{}
	heap.Init(h)
	return &PriorityQueue{heap: h}
}

// Enqueue adds a task to the queue
func (pq *PriorityQueue) Enqueue(task *domain.SyncTask) error {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	// Assign unique ID if not set
	if task.ID == 0 {
		pq.idCounter++
		task.ID = pq.idCounter
	}

	// Set timestamps if not set
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now()
	}
	if task.UpdatedAt.IsZero() {
		task.UpdatedAt = time.Now()
	}

	// Set default priority if not set
	if task.Priority == 0 {
		task.Priority = domain.NormalPriority
	}

	// Set default status if not set
	if task.Status == 0 {
		task.Status = domain.TaskPending
	}

	heap.Push(pq.heap, task)
	return nil
}

// Dequeue removes and returns the highest priority task
func (pq *PriorityQueue) Dequeue() (*domain.SyncTask, error) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	if pq.heap.Len() == 0 {
		return nil, fmt.Errorf("queue is empty")
	}

	task := heap.Pop(pq.heap).(*domain.SyncTask)
	task.Status = domain.TaskRunning
	task.UpdatedAt = time.Now()

	return task, nil
}

// GetNextTasks returns the next N tasks without removing them
func (pq *PriorityQueue) GetNextTasks(limit int) []domain.SyncTask {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	if limit <= 0 {
		limit = 10
	}

	tasks := make([]domain.SyncTask, 0, limit)
	tempHeap := make([]*domain.SyncTask, 0, pq.heap.Len())

	// Extract tasks up to limit
	for i := 0; i < limit && pq.heap.Len() > 0; i++ {
		task := heap.Pop(pq.heap).(*domain.SyncTask)
		tasks = append(tasks, *task)
		tempHeap = append(tempHeap, task)
	}

	// Put tasks back
	for _, task := range tempHeap {
		heap.Push(pq.heap, task)
	}

	return tasks
}

// UpdateTask updates a task in the queue
func (pq *PriorityQueue) UpdateTask(task *domain.SyncTask) error {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	// Find and update the task
	for i := 0; i < pq.heap.Len(); i++ {
		if (*pq.heap)[i].ID == task.ID {
			(*pq.heap)[i] = task
			heap.Fix(pq.heap, i)
			return nil
		}
	}

	return fmt.Errorf("task not found: %d", task.ID)
}

// DeleteTask removes a task from the queue
func (pq *PriorityQueue) DeleteTask(id int64) error {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	// Find and remove the task
	for i := 0; i < pq.heap.Len(); i++ {
		if (*pq.heap)[i].ID == id {
			heap.Remove(pq.heap, i)
			return nil
		}
	}

	return fmt.Errorf("task not found: %d", id)
}

// CountByPriority returns the count of tasks with a specific priority
func (pq *PriorityQueue) CountByPriority(priority domain.SyncPriority) (int, error) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	count := 0
	for _, task := range *pq.heap {
		if task.Priority == priority {
			count++
		}
	}

	return count, nil
}

// Count returns the total number of tasks in the queue
func (pq *PriorityQueue) Count() int {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	return pq.heap.Len()
}

// GetCriticalTasks returns all critical priority tasks
func (pq *PriorityQueue) GetCriticalTasks() []domain.SyncTask {
	return pq.GetTasksByPriority(domain.CriticalPriority)
}

// GetHighPriorityTasks returns all high priority tasks
func (pq *PriorityQueue) GetHighPriorityTasks() []domain.SyncTask {
	return pq.GetTasksByPriority(domain.HighPriority)
}

// GetTasksByPriority returns all tasks with a specific priority
func (pq *PriorityQueue) GetTasksByPriority(priority domain.SyncPriority) []domain.SyncTask {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	var tasks []domain.SyncTask
	for _, task := range *pq.heap {
		if task.Priority == priority {
			tasks = append(tasks, *task)
		}
	}

	return tasks
}

// FindTaskByID finds a task by its ID without removing it
func (pq *PriorityQueue) FindTaskByID(id int64) (*domain.SyncTask, error) {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	for _, task := range *pq.heap {
		if task.ID == id {
			return task, nil
		}
	}

	return nil, fmt.Errorf("task not found: %d", id)
}

// Clear removes all tasks from the queue
func (pq *PriorityQueue) Clear() {
	pq.mu.Lock()
	defer pq.mu.Unlock()
	pq.heap = &TaskHeap{}
	heap.Init(pq.heap)
}

// ToSlice returns a copy of all tasks in the queue
func (pq *PriorityQueue) ToSlice() []domain.SyncTask {
	pq.mu.Lock()
	defer pq.mu.Unlock()

	tasks := make([]domain.SyncTask, 0, pq.heap.Len())
	for _, task := range *pq.heap {
		tasks = append(tasks, *task)
	}

	return tasks
}

// NewSyncTask creates a new sync task with default values
func NewSyncTask(syncConfigID int64, filePath, remoteID string, fileSize int64, taskType domain.SyncTaskType) *domain.SyncTask {
	return &domain.SyncTask{
		SyncConfigID: syncConfigID,
		FilePath:     filePath,
		RemoteID:     remoteID,
		FileSize:     fileSize,
		TaskType:     taskType,
		Priority:     domain.NormalPriority,
		Status:       domain.TaskPending,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
}

// NewCriticalTask creates a critical priority sync task
func NewCriticalTask(syncConfigID int64, filePath, remoteID string, fileSize int64, taskType domain.SyncTaskType) *domain.SyncTask {
	task := NewSyncTask(syncConfigID, filePath, remoteID, fileSize, taskType)
	task.Priority = domain.CriticalPriority
	return task
}

// NewHighPriorityTask creates a high priority sync task
func NewHighPriorityTask(syncConfigID int64, filePath, remoteID string, fileSize int64, taskType domain.SyncTaskType) *domain.SyncTask {
	task := NewSyncTask(syncConfigID, filePath, remoteID, fileSize, taskType)
	task.Priority = domain.HighPriority
	return task
}

// NewLowPriorityTask creates a low priority sync task
func NewLowPriorityTask(syncConfigID int64, filePath, remoteID string, fileSize int64, taskType domain.SyncTaskType) *domain.SyncTask {
	task := NewSyncTask(syncConfigID, filePath, remoteID, fileSize, taskType)
	task.Priority = domain.LowPriority
	return task
}
