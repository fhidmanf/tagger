package controllers

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	imageinf "github.com/ricardomaraschini/tagger/imagetags/generated/informers/externalversions"
	imagelis "github.com/ricardomaraschini/tagger/imagetags/generated/listers/imagetags/v1"
	imagtagv1 "github.com/ricardomaraschini/tagger/imagetags/v1"
)

// TagUpdater abstraction exists to make testing easier. You most likely wanna
// see Tag struct under services/tag.go for a concrete implementation of this.
type TagUpdater interface {
	Update(context.Context, *imagtagv1.Tag) error
}

// Tag controller handles events related to Tags. It starts and receives events
// from the informer, calling appropriate functions on our concrete services
// layer implementation.
type Tag struct {
	taglister imagelis.TagLister
	queue     workqueue.RateLimitingInterface
	tagsvc    TagUpdater
	appctx    context.Context
	tokens    chan bool
}

// NewTag returns a new controller for Image Tags. This controller runs image
// tag imports in parallel, at a given time we can have at max "workers"
// distinct image tags being processed.
func NewTag(
	taginf imageinf.SharedInformerFactory, tagsvc TagUpdater, workers int,
) *Tag {
	ratelimit := workqueue.NewItemExponentialFailureRateLimiter(time.Second, time.Minute)
	ctrl := &Tag{
		taglister: taginf.Images().V1().Tags().Lister(),
		queue:     workqueue.NewRateLimitingQueue(ratelimit),
		tagsvc:    tagsvc,
		tokens:    make(chan bool, workers),
	}
	taginf.Images().V1().Tags().Informer().AddEventHandler(ctrl.handlers())
	return ctrl
}

// Name returns a name identifier for this controller.
func (t *Tag) Name() string {
	return "tag"
}

// enqueueEvent generates a key using "namespace/name" for the event received
// and then enqueues this index to be processed.
func (t *Tag) enqueueEvent(o interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(o)
	if err != nil {
		klog.Errorf("fail to enqueue event: %v : %s", o, err)
		return
	}
	t.queue.AddRateLimited(key)
}

// handlers return a event handler that will be called by the informer
// whenever an event occurs. This handler basically enqueues everything
// in our work queue.
func (t *Tag) handlers() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(o interface{}) {
			t.enqueueEvent(o)
		},
		UpdateFunc: func(o, n interface{}) {
			t.enqueueEvent(o)
		},
		DeleteFunc: func(o interface{}) {
			t.enqueueEvent(o)
		},
	}
}

// eventProcessor reads our events calling syncTag for all of them.
func (t *Tag) eventProcessor(wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		evt, end := t.queue.Get()
		if end {
			return
		}

		t.tokens <- true
		go func() {
			defer func() {
				<-t.tokens
			}()

			namespace, name, err := cache.SplitMetaNamespaceKey(evt.(string))
			if err != nil {
				klog.Errorf("invalid event received %s: %s", evt, err)
				t.queue.Done(evt)
				return
			}

			klog.Infof("received event for tag: %s", evt)
			if err := t.syncTag(namespace, name); err != nil {
				klog.Errorf("error processing tag %s: %v", evt, err)
				t.queue.Done(evt)
				t.queue.AddRateLimited(evt)
				return
			}

			klog.Infof("event for tag %s processed", evt)
			t.queue.Done(evt)
			t.queue.Forget(evt)
		}()
	}
}

// syncTag process an event for an image stream. A max of three minutes is
// allowed per image stream sync.
func (t *Tag) syncTag(namespace, name string) error {
	ctx, cancel := context.WithTimeout(t.appctx, 3*time.Minute)
	defer cancel()

	it, err := t.taglister.Tags(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	it = it.DeepCopy()
	return t.tagsvc.Update(ctx, it)
}

// Start starts the controller's event loop.
func (t *Tag) Start(ctx context.Context) error {
	// appctx is the 'keep going' context, if it is cancelled
	// everything we might be doing should stop.
	t.appctx = ctx

	var wg sync.WaitGroup
	wg.Add(1)
	go t.eventProcessor(&wg)

	// wait until it is time to die.
	<-t.appctx.Done()

	t.queue.ShutDown()
	wg.Wait()
	return nil
}
