package main

// device is what the registry, the lanes' hooks, refresh and autotest need from a
// configured Grenton object (Light, Thermo, Shutter, MotionSensor).
type device interface {
	// base returns the common part: identity (Clu, Id, Kind, Name) and the parent CLU.
	base() *CluObject
	// readRequest returns the item sent to the read endpoint for this device.
	readRequest() ReqObject
	// applyRead validates a read item and applies it under the device lock if the ordering
	// rules allow (concept 3.3 rule 4: sentGen is the device generation recorded when the
	// read was sent). It returns ApplyInvalid (with the reason), ApplyDiscarded, or
	// ApplyChanged/ApplyUnchanged (the first valid read always counts as a change).
	// stamp is the completion stamp of the response. It does not touch HomeKit.
	applyRead(obj ReqObject, sentGen, stamp uint64) (ApplyOutcome, error)
	// Sync pushes the device state to HomeKit. Never called with the device lock held.
	Sync()
}

func (co *CluObject) base() *CluObject { return co }

func (co *CluObject) readRequest() ReqObject { return co.Req }

// registry finds devices by Clu + Id (the mixed id, e.g. "DOU0001"), case-insensitive.
type registry struct {
	byKey map[string]device
	all   []device
}

// newRegistry indexes every device of the CLUs, in configuration order (per CLU: lights,
// thermostats, shutters, motion sensors). A second device with the same Clu + Id is
// reported through logf and left out: reads and pushes could not tell the two apart.
func newRegistry(clus []*Clu, logf func(string, ...any)) *registry {
	r := &registry{byKey: map[string]device{}}
	add := func(clu *Clu, d device) {
		key := entryKey(clu.Id, d.base().GetMixedId())
		if first, ok := r.byKey[key]; ok {
			logf("WARNING: device %q (%s|%s) has the same CLU and id as %q; ignoring the second one",
				d.base().Name, clu.Id, d.base().GetMixedId(), first.base().Name)
			return
		}
		r.byKey[key] = d
		r.all = append(r.all, d)
	}
	for _, clu := range clus {
		for _, d := range clu.Lights {
			add(clu, d)
		}
		for _, d := range clu.Therms {
			add(clu, d)
		}
		for _, d := range clu.Shutters {
			add(clu, d)
		}
		for _, d := range clu.MotionSensors {
			add(clu, d)
		}
	}
	return r
}

// find returns the device with the given CLU id and mixed id.
func (r *registry) find(clu, id string) (device, bool) {
	d, ok := r.byKey[entryKey(clu, id)]
	return d, ok
}

// devices returns every registered device in configuration order.
func (r *registry) devices() []device { return r.all }
