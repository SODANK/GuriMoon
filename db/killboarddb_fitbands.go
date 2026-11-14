package db

// Fitting bands: the shared grouping behind every "show me the fit" renderer. html/template
// cannot group a sorted range, so FitBands does it once for both renderers. It also sums the
// per-band ISK, which neither can derive from the head row's whole-ship Dropped/DestroyedValue.

// KillmailFitBand is one slot band of a fit (High, Mid, Low, Rig, Drone Bay, Cargo, Implant or Other)
// with the items that sat in it.
type KillmailFitBand struct {
	Label          string
	Items          []KillmailItemRow
	DroppedValue   float64
	DestroyedValue float64
}

// TotalValue is the whole band, dropped and destroyed together.
func (b KillmailFitBand) TotalValue() float64 { return b.DroppedValue + b.DestroyedValue }

// FitBands groups FittedItems into its slot bands, in fitting order. A band is emitted only
// if something sat in it, so an empty rack never prints a heading over nothing.
func (d *KillmailDetail) FitBands() []KillmailFitBand {
	items := d.FittedItems()
	if len(items) == 0 {
		return nil
	}
	var out []KillmailFitBand
	for _, it := range items {
		label := it.Placement()
		if len(out) == 0 || out[len(out)-1].Label != label {
			out = append(out, KillmailFitBand{Label: label})
		}
		band := &out[len(out)-1]
		band.Items = append(band.Items, it)
		if it.Dropped {
			band.DroppedValue += it.TotalValue
		} else {
			band.DestroyedValue += it.TotalValue
		}
	}
	return out
}
