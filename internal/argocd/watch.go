package argocd

import (
	"context"
	"errors"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/config"
)

var (
	applications  = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "applications"}
	imageUpdaters = schema.GroupVersionResource{Group: "argocd-image-updater.argoproj.io", Version: "v1alpha1", Resource: "imageupdaters"}
)

const resync = 10 * time.Minute

// Watch calls handle for every add and update of a tracked Application until ctx ends. The
// informers list, watch and re-list on their own (including on 410 Gone), so no event is missed
// while connected. synced is called once after the initial list was delivered.
//
// ImageUpdater CRs are loaded into iu first, so Applications can be matched against them. If
// the CRD is missing or not readable, only annotated Applications are tracked.
func Watch(ctx context.Context, rc *rest.Config, cfg *config.Config, log *slog.Logger,
	iu *Store, handle func(*App), synced func()) error {
	dc, err := dynamic.NewForConfig(rc)
	if err != nil {
		return err
	}

	if err := watchImageUpdaters(ctx, dc, cfg, log, iu); err != nil {
		return err
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dc, resync, cfg.ArgoCDNamespace, nil)
	inf := factory.ForResource(applications).Informer()
	on := func(obj any) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return
		}
		a, err := Parse(u, cfg, iu)
		if err != nil {
			log.Warn("skipping application", "app", u.GetName(), "error", err)
			return
		}
		if a != nil {
			handle(a)
		}
	}
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    on,
		UpdateFunc: func(_, n any) { on(n) },
	}); err != nil {
		return err
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
		return errors.New("argocd application cache did not sync")
	}
	synced()
	<-ctx.Done()
	return nil
}

// watchImageUpdaters fills iu from the ImageUpdater CRs and waits for the initial list.
func watchImageUpdaters(ctx context.Context, dc dynamic.Interface, cfg *config.Config, log *slog.Logger, iu *Store) error {
	_, err := dc.Resource(imageUpdaters).Namespace(cfg.ArgoCDNamespace).List(ctx, metav1.ListOptions{Limit: 1})
	switch {
	case apierrors.IsNotFound(err) || apierrors.IsForbidden(err):
		log.Info("image updater CRs not available, tracking annotated applications only", "reason", err.Error())
		return nil
	case err != nil:
		return err
	}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dc, resync, cfg.ArgoCDNamespace, nil)
	inf := factory.ForResource(imageUpdaters).Informer()
	set := func(obj any) {
		if u, ok := obj.(*unstructured.Unstructured); ok {
			iu.Set(u)
		}
	}
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    set,
		UpdateFunc: func(_, n any) { set(n) },
		DeleteFunc: func(obj any) {
			if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = t.Obj
			}
			if u, ok := obj.(*unstructured.Unstructured); ok {
				iu.Delete(u.GetName())
			}
		},
	}); err != nil {
		return err
	}
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
		return errors.New("image updater cache did not sync")
	}
	return nil
}
