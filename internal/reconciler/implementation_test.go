package reconciler_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/nginxinc/nginx-kubernetes-gateway/internal/reconciler/reconcilerfakes"

	"github.com/nginxinc/nginx-kubernetes-gateway/internal/events"
	"github.com/nginxinc/nginx-kubernetes-gateway/internal/reconciler"
)

type getFunc func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error

type result struct {
	err             error
	reconcileResult reconcile.Result
}

var _ = Describe("Reconciler", func() {
	var (
		rec        *reconciler.Implementation
		fakeGetter *reconcilerfakes.FakeGetter
		eventCh    chan interface{}

		hr1NsName = types.NamespacedName{
			Namespace: "test",
			Name:      "hr-1",
		}

		hr1 = &v1beta1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: hr1NsName.Namespace,
				Name:      hr1NsName.Name,
			},
		}

		hr2NsName = types.NamespacedName{
			Namespace: "test",
			Name:      "hr-2",
		}

		hr2 = &v1beta1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: hr2NsName.Namespace,
				Name:      hr2NsName.Name,
			},
		}

		hr2IsInvalidValidator = func(obj client.Object) error {
			if client.ObjectKeyFromObject(obj) == hr2NsName {
				return errors.New("test")
			}
			return nil
		}
	)

	getReturnsHRForHR := func(hr *v1beta1.HTTPRoute) getFunc {
		return func(
			ctx context.Context,
			nsname types.NamespacedName,
			object client.Object,
			option ...client.GetOption,
		) error {
			Expect(object).To(BeAssignableToTypeOf(&v1beta1.HTTPRoute{}))
			Expect(nsname).To(Equal(client.ObjectKeyFromObject(hr)))

			hr.DeepCopyInto(object.(*v1beta1.HTTPRoute))

			return nil
		}
	}

	getReturnsNotFoundErrorForHR := func(hr *v1beta1.HTTPRoute) getFunc {
		return func(
			ctx context.Context,
			nsname types.NamespacedName,
			object client.Object,
			option ...client.GetOption,
		) error {
			Expect(object).To(BeAssignableToTypeOf(&v1beta1.HTTPRoute{}))
			Expect(nsname).To(Equal(client.ObjectKeyFromObject(hr)))

			return apierrors.NewNotFound(schema.GroupResource{}, "not found")
		}
	}

	startReconcilingWithContext := func(ctx context.Context, nsname types.NamespacedName) <-chan result {
		resultCh := make(chan result)

		go func() {
			defer GinkgoRecover()

			res, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: nsname})
			resultCh <- result{err: err, reconcileResult: res}

			close(resultCh)
		}()

		return resultCh
	}

	startReconciling := func(nsname types.NamespacedName) <-chan result {
		return startReconcilingWithContext(context.Background(), nsname)
	}

	BeforeEach(func() {
		fakeGetter = &reconcilerfakes.FakeGetter{}
		eventCh = make(chan interface{})
	})

	Describe("Normal cases", func() {
		testUpsert := func(hr *v1beta1.HTTPRoute) {
			fakeGetter.GetCalls(getReturnsHRForHR(hr))

			resultCh := startReconciling(client.ObjectKeyFromObject(hr))

			Eventually(eventCh).Should(Receive(Equal(&events.UpsertEvent{Resource: hr})))
			Eventually(resultCh).Should(Receive(Equal(result{err: nil, reconcileResult: reconcile.Result{}})))
		}

		testDelete := func(hr *v1beta1.HTTPRoute) {
			fakeGetter.GetCalls(getReturnsNotFoundErrorForHR(hr))

			resultCh := startReconciling(client.ObjectKeyFromObject(hr))

			Eventually(eventCh).Should(Receive(Equal(&events.DeleteEvent{
				NamespacedName: client.ObjectKeyFromObject(hr),
				Type:           &v1beta1.HTTPRoute{},
			})))
			Eventually(resultCh).Should(Receive(Equal(result{err: nil, reconcileResult: reconcile.Result{}})))
		}

		When("Reconciler doesn't have a filter", func() {
			BeforeEach(func() {
				rec = reconciler.NewImplementation(reconciler.Config{
					Getter:     fakeGetter,
					ObjectType: &v1beta1.HTTPRoute{},
					EventCh:    eventCh,
				})
			})

			It("should upsert HTTPRoute", func() {
				testUpsert(hr1)
			})

			It("should delete HTTPRoute", func() {
				testDelete(hr1)
			})
		})

		When("Reconciler has a NamespacedNameFilter", func() {
			BeforeEach(func() {
				filter := func(nsname types.NamespacedName) (bool, string) {
					if nsname != hr1NsName {
						return false, "ignore"
					}
					return true, ""
				}

				rec = reconciler.NewImplementation(reconciler.Config{
					Getter:               fakeGetter,
					ObjectType:           &v1beta1.HTTPRoute{},
					EventCh:              eventCh,
					NamespacedNameFilter: filter,
				})
			})

			When("HTTPRoute is not ignored", func() {
				It("should upsert HTTPRoute", func() {
					testUpsert(hr1)
				})

				It("should delete HTTPRoute", func() {
					testDelete(hr1)
				})
			})

			When("HTTPRoute is ignored", func() {
				It("should not upsert HTTPRoute", func() {
					fakeGetter.GetCalls(getReturnsHRForHR(hr2))

					resultCh := startReconciling(hr2NsName)

					Consistently(eventCh).ShouldNot(Receive())
					Eventually(resultCh).Should(Receive(Equal(result{err: nil, reconcileResult: reconcile.Result{}})))
				})

				It("should not delete HTTPRoute", func() {
					fakeGetter.GetCalls(getReturnsNotFoundErrorForHR(hr2))

					resultCh := startReconciling(hr2NsName)

					Consistently(eventCh).ShouldNot(Receive())
					Eventually(resultCh).Should(Receive(Equal(result{err: nil, reconcileResult: reconcile.Result{}})))
				})
			})
		})

		When("Reconciler includes a Webhook Validator", func() {
			var fakeRecorder *reconcilerfakes.FakeEventRecorder

			BeforeEach(func() {
				fakeRecorder = &reconcilerfakes.FakeEventRecorder{}

				rec = reconciler.NewImplementation(reconciler.Config{
					Getter:           fakeGetter,
					ObjectType:       &v1beta1.HTTPRoute{},
					EventCh:          eventCh,
					WebhookValidator: hr2IsInvalidValidator,
					EventRecorder:    fakeRecorder,
				})
			})

			It("should upsert valid HTTPRoute", func() {
				testUpsert(hr1)
				Expect(fakeRecorder.EventfCallCount()).To(Equal(0))
			})

			It("should reject invalid HTTPRoute", func() {
				fakeGetter.GetCalls(getReturnsHRForHR(hr2))

				resultCh := startReconciling(client.ObjectKeyFromObject(hr2))

				Eventually(eventCh).Should(Receive(Equal(&events.DeleteEvent{
					NamespacedName: client.ObjectKeyFromObject(hr2),
					Type:           &v1beta1.HTTPRoute{},
				})))
				Eventually(resultCh).Should(Receive(Equal(result{err: nil, reconcileResult: reconcile.Result{}})))

				Expect(fakeRecorder.EventfCallCount()).To(Equal(1))
				obj, _, _, _, _ := fakeRecorder.EventfArgsForCall(0)
				Expect(obj).To(Equal(hr2))
			})

			It("should delete HTTPRoutes", func() {
				testDelete(hr1)
				testDelete(hr2)
				Expect(fakeRecorder.EventfCallCount()).To(Equal(0))
			})
		})
	})

	Describe("Edge cases", func() {
		var fakeRecorder *reconcilerfakes.FakeEventRecorder

		BeforeEach(func() {
			fakeRecorder = &reconcilerfakes.FakeEventRecorder{}

			rec = reconciler.NewImplementation(reconciler.Config{
				Getter:           fakeGetter,
				ObjectType:       &v1beta1.HTTPRoute{},
				EventCh:          eventCh,
				WebhookValidator: hr2IsInvalidValidator,
				EventRecorder:    fakeRecorder,
			})
		})

		It("should not reconcile when Getter returns error", func() {
			getError := errors.New("get error")
			fakeGetter.GetReturns(getError)

			resultCh := startReconciling(hr1NsName)

			Consistently(eventCh).ShouldNot(Receive())
			Eventually(resultCh).Should(Receive(Equal(result{err: getError, reconcileResult: reconcile.Result{}})))
		})

		DescribeTable("Reconciler should not block when ctx is done",
			func(get getFunc, invalidResourceEventCount int, nsname types.NamespacedName) {
				fakeGetter.GetCalls(get)

				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				resultCh := startReconcilingWithContext(ctx, nsname)

				Consistently(eventCh).ShouldNot(Receive())
				Expect(resultCh).To(Receive(Equal(result{err: nil, reconcileResult: reconcile.Result{}})))
				Expect(fakeRecorder.EventfCallCount()).To(Equal(invalidResourceEventCount))
			},
			Entry("Upserting valid HTTPRoute", getReturnsHRForHR(hr1), 0, hr1NsName),
			Entry("Deleting valid HTTPRoute", getReturnsNotFoundErrorForHR(hr1), 0, hr1NsName),
			Entry("Upserting invalid HTTPRoute", getReturnsHRForHR(hr2), 1, hr2NsName),
		)
	})
})
