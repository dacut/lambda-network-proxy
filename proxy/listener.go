package main

import (
	"context"
	"sync"
)

type Listener interface {
	Run(ctx context.Context, wg *sync.WaitGroup)
	Stop()
}
