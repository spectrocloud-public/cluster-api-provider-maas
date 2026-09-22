package controllers

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2/klogr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// drainEvents reads whatever the FakeRecorder buffered without blocking.
func drainEvents(rec *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-rec.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

// The EvacuationControllerMissing warning is the only signal an operator gets
// that deletion is proceeding without evacuation; it must accompany the orphaned
// finalizer drop (PCP-7660).
func TestReconcileDeleteWarnsEvacuationControllerMissing(t *testing.T) {
	g := NewGomegaWithT(t)
	t.Setenv("MAAS_ENDPOINT", "http://localhost:5240/MAAS")
	t.Setenv("MAAS_API_KEY", "test:key:secret")

	machine := evacuationTestMachine("test-maasmachine", true, nil, true)
	_, ms, _ := newEvacuationTestScopes(t, lxdEnabledCluster(true), machine)

	rec := record.NewFakeRecorder(8)
	r := &MaasMachineReconciler{
		Log:        klogr.New(),
		Recorder:   rec,
		HMCEnabled: false, // HMC not deployed in this process
	}
	_, err := r.reconcileDelete(context.Background(), ms, ms.ClusterScope)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(ms.Close()).To(Succeed())

	events := drainEvents(rec)
	g.Expect(events).To(HaveLen(1),
		"dropping the orphaned evacuation finalizer must warn exactly once")
	g.Expect(events[0]).To(ContainSubstring("EvacuationControllerMissing"))
	g.Expect(events[0]).To(ContainSubstring("releasing host without evacuation"))
}

// HMC deployed: the finalizer is legitimately owned, so delete requeues for
// evacuation and must stay silent — an event here would fire every 10s requeue.
func TestReconcileDeleteStaysSilentWhenHMCEnabled(t *testing.T) {
	g := NewGomegaWithT(t)
	t.Setenv("MAAS_ENDPOINT", "http://localhost:5240/MAAS")
	t.Setenv("MAAS_API_KEY", "test:key:secret")

	machine := evacuationTestMachine("test-maasmachine", true, nil, true)
	_, ms, _ := newEvacuationTestScopes(t, lxdEnabledCluster(true), machine)

	rec := record.NewFakeRecorder(8)
	r := &MaasMachineReconciler{
		Log:        klogr.New(),
		Recorder:   rec,
		HMCEnabled: true,
	}
	result, err := r.reconcileDelete(context.Background(), ms, ms.ClusterScope)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(10 * time.Second))
	g.Expect(drainEvents(rec)).To(BeEmpty(),
		"the HMC-owned requeue path must not emit the missing-controller warning")

	g.Expect(ms.Close()).To(Succeed())
	g.Expect(controllerutil.ContainsFinalizer(ms.MaasMachine, HostEvacuationFinalizer)).To(BeTrue())
}
