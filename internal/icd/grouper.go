package icd

import (
	"bufio"
	"encoding/csv"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"sync"
)

// ── public request / response types ──────────────────────────────────────────

// GroupRequest is the input for the DRG grouper.
type GroupRequest struct {
	PrincipalDx     string   `json:"principal_dx"`
	SecondaryDxs    []string `json:"secondary_dxs"`
	Procedures      []string `json:"procedures"`
	Age             int      `json:"age"`
	Sex             string   `json:"sex"`              // "M" | "F" | "U"
	DischargeStatus string   `json:"discharge_status"` // UHDDS code, default "01"
}

// DiagnosisDetail is a single diagnosis entry in the grouper result.
type DiagnosisDetail struct {
	Code        string `json:"code"`
	Description string `json:"description"`
	IsPrincipal bool   `json:"is_principal"`
}

// ProcedureDetail is a single procedure entry in the grouper result.
type ProcedureDetail struct {
	Code        string `json:"code"`
	Description string `json:"description"`
}

// EncounterInfo captures the demographic/discharge context.
type EncounterInfo struct {
	Age             int    `json:"age"`
	Sex             string `json:"sex"`
	DischargeStatus string `json:"discharge_status"`
}

// GroupResult is returned by the DRG grouper.
type GroupResult struct {
	DRGCode           string            `json:"drg_code"`
	Description       string            `json:"description"`
	MDC               string            `json:"mdc"`
	MDCDescription    string            `json:"mdc_description"`
	Weight            float64           `json:"weight"`
	GeometricLOS      *float64          `json:"geometric_los"`
	ArithmeticLOS     *float64          `json:"arithmetic_los"`
	Partition         string            `json:"partition"`          // "SURG" | "MED"
	ComplicationLevel string            `json:"complication_level"` // "MCC" | "CC" | "NONE"
	IsPreMDC          bool              `json:"is_pre_mdc"`
	PrincipalDiagnosis DiagnosisDetail  `json:"principal_diagnosis"`
	SecondaryDiagnoses []DiagnosisDetail `json:"secondary_diagnoses"`
	Procedures         []ProcedureDetail `json:"procedures"`
	Encounter          EncounterInfo     `json:"encounter"`
	HasORProcedure    bool              `json:"has_or_procedure"`
	CCCodesApplied    []string          `json:"cc_codes_applied"`
	MCCCodesApplied   []string          `json:"mcc_codes_applied"`
}

// ── Grouper ───────────────────────────────────────────────────────────────────

// Grouper holds in-memory reference data for MS-DRG assignment and is safe for
// concurrent use after Load() has been called.
type Grouper struct {
	store *Store

	once     sync.Once
	loadErr  error
	mccCodes map[string]bool
	ccCodes  map[string]bool
	// exclusions[cc_code] = set of principal-dx codes that nullify it
	exclusions  map[string]map[string]bool
	pcsDesc     map[string]string // ICD-10-PCS code -> description
}

// NewGrouper creates a Grouper backed by store for DRG lookups.
// Call Load(fsys) before using Group().
func NewGrouper(store *Store) *Grouper {
	return &Grouper{store: store}
}

// Load reads the reference data files from fsys. It is idempotent and safe to
// call multiple times; only the first call does any work.
func (g *Grouper) Load(fsys fs.FS) error {
	g.once.Do(func() {
		g.loadErr = g.load(fsys)
	})
	return g.loadErr
}

func (g *Grouper) load(fsys fs.FS) error {
	var err error

	g.mccCodes, err = loadSeverityList(fsys, "data/official/mcc_list.txt")
	if err != nil {
		return err
	}
	g.ccCodes, err = loadSeverityList(fsys, "data/official/cc_list.txt")
	if err != nil {
		return err
	}
	g.exclusions, err = loadCCExclusions(fsys, "data/official/cc_exclusions.txt")
	if err != nil {
		return err
	}
	g.pcsDesc, err = loadPCSDescriptions(fsys, "data/official/icd_10_pcs.csv")
	if err != nil {
		return err
	}
	return nil
}

// ── Group ─────────────────────────────────────────────────────────────────────

// Group assigns an MS-DRG for the given encounter.
func (g *Grouper) Group(req GroupRequest) GroupResult {
	// normalise codes
	pdx := norm(req.PrincipalDx)
	sdxs := make([]string, 0, len(req.SecondaryDxs))
	for _, c := range req.SecondaryDxs {
		if n := norm(c); n != "" {
			sdxs = append(sdxs, n)
		}
	}
	procs := make([]string, 0, len(req.Procedures))
	for _, c := range req.Procedures {
		if n := norm(c); n != "" {
			procs = append(procs, n)
		}
	}

	ds := req.DischargeStatus
	if ds == "" {
		ds = "01"
	}

	encounter := EncounterInfo{Age: req.Age, Sex: req.Sex, DischargeStatus: ds}

	// build detail objects
	pdxDetail := DiagnosisDetail{Code: pdx, Description: g.diagDesc(pdx), IsPrincipal: true}
	sdxDetails := make([]DiagnosisDetail, len(sdxs))
	for i, c := range sdxs {
		sdxDetails[i] = DiagnosisDetail{Code: c, Description: g.diagDesc(c)}
	}
	procDetails := make([]ProcedureDetail, len(procs))
	for i, c := range procs {
		procDetails[i] = ProcedureDetail{Code: c, Description: g.pcsDescription(c)}
	}

	// step 1 – pre-MDC
	ccLevel, ccList, mccList := g.resolveComplications(pdx, sdxs, ds)
	if pre := g.evaluatePreMDC(pdx, procs, ccLevel, ccList, mccList, pdxDetail, sdxDetails, procDetails, encounter); pre != nil {
		return *pre
	}

	// step 2 – MDC from principal dx
	mdcCode, mdcDesc := classifyMDC(pdx)
	if mdcDesc == "" {
		if e, ok := g.store.LookupMDC(mdcCode); ok {
			mdcDesc = e.Description
		}
	}

	// step 3 – surgical / medical partition
	hasOR, partition := g.partitionProcs(procs)

	// step 4 – complication severity (already computed above, reuse)
	_ = hasOR

	// step 5 – DRG selection
	drgCode, drgDesc, wt, geo, arith := g.selectDRG(mdcCode, partition, ccLevel, procs, pdx)

	return GroupResult{
		DRGCode:            drgCode,
		Description:        drgDesc,
		MDC:                mdcCode,
		MDCDescription:     mdcDesc,
		Weight:             wt,
		GeometricLOS:       geo,
		ArithmeticLOS:      arith,
		Partition:          partition,
		ComplicationLevel:  ccLevel,
		IsPreMDC:           false,
		PrincipalDiagnosis: pdxDetail,
		SecondaryDiagnoses: sdxDetails,
		Procedures:         procDetails,
		Encounter:          encounter,
		HasORProcedure:     hasOR,
		CCCodesApplied:     nonNilStrings(ccList),
		MCCCodesApplied:    nonNilStrings(mccList),
	}
}

// ── step 1: pre-MDC ──────────────────────────────────────────────────────────

func (g *Grouper) evaluatePreMDC(
	pdx string,
	procs []string,
	ccLevel string,
	ccList, mccList []string,
	pdxDetail DiagnosisDetail,
	sdxDetails []DiagnosisDetail,
	procDetails []ProcedureDetail,
	encounter EncounterInfo,
) *GroupResult {
	hasMCC := ccLevel == "MCC"
	hasCC := ccLevel == "CC"

	var drg string

	for _, code := range procs {
		if len(code) != 7 {
			continue
		}
		sec := string(code[0])
		bsys := string(code[1])
		rop := string(code[2])
		bpart := string(code[3])
		dev := string(code[5])

		// heart transplant / heart assist (001-002)
		if sec == "0" && bsys == "2" && rop == "Y" && bpart == "A" {
			drg = ternary(hasMCC, "001", "002")
			break
		}
		if sec == "0" && bsys == "2" && rop == "H" && (dev == "Q" || dev == "R" || dev == "S") {
			drg = ternary(hasMCC, "001", "002")
			break
		}
		// liver transplant (005-006)
		if sec == "0" && bsys == "F" && rop == "Y" && bpart != "G" {
			drg = ternary(hasMCC, "005", "006")
			break
		}
		// pancreas + kidney transplant (008 vs 010)
		if sec == "0" && bsys == "F" && rop == "Y" && bpart == "G" {
			hasKidney := false
			for _, p := range procs {
				if len(p) >= 3 && p[0:3] == "0TY" {
					hasKidney = true
					break
				}
			}
			drg = ternary(hasKidney, "008", "010")
			break
		}
		// kidney transplant
		if sec == "0" && bsys == "T" && rop == "Y" {
			hasPancreas := false
			for _, p := range procs {
				if len(p) >= 4 && p[0:3] == "0FY" && string(p[3]) == "G" {
					hasPancreas = true
					break
				}
			}
			if hasPancreas {
				drg = "008"
			}
			break
		}
		// lung transplant (007)
		if sec == "0" && bsys == "B" && rop == "Y" {
			drg = "007"
			break
		}
		// tracheostomy (003-004 or 011-013)
		if sec == "0" && bsys == "B" && rop == "1" && bpart == "1" {
			if isFaceMouthNeckDx(pdx) {
				if hasMCC {
					drg = "011"
				} else if hasCC {
					drg = "012"
				} else {
					drg = "013"
				}
			} else {
				drg = ternary(hasMCC, "003", "004")
			}
			break
		}
		// ECMO (003)
		if sec == "5" && bsys == "A" && rop == "1" && bpart == "5" {
			drg = "003"
			break
		}
		// bone marrow / stem cell transplant (014)
		if sec == "3" && bsys == "0" && rop == "2" && (dev == "G" || dev == "X") {
			drg = "014"
			break
		}
	}

	if drg == "" {
		return nil
	}

	ref, _ := g.store.LookupDRG(drg)
	geo := ref.GeometricLOS
	arith := ref.ArithmeticLOS
	var geoPtr, arithPtr *float64
	if geo > 0 {
		geoPtr = &geo
	}
	if arith > 0 {
		arithPtr = &arith
	}

	return &GroupResult{
		DRGCode:            drg,
		Description:        ref.Description,
		MDC:                "PRE",
		MDCDescription:     "Pre-MDC",
		Weight:             ref.Weight,
		GeometricLOS:       geoPtr,
		ArithmeticLOS:      arithPtr,
		Partition:          "SURG",
		ComplicationLevel:  ccLevel,
		IsPreMDC:           true,
		PrincipalDiagnosis: pdxDetail,
		SecondaryDiagnoses: sdxDetails,
		Procedures:         procDetails,
		Encounter:          encounter,
		HasORProcedure:     true,
		CCCodesApplied:     nonNilStrings(ccList),
		MCCCodesApplied:    nonNilStrings(mccList),
	}
}

func isFaceMouthNeckDx(code string) bool {
	return strings.HasPrefix(code, "J") || strings.HasPrefix(code, "K") ||
		strings.HasPrefix(code, "C0") || strings.HasPrefix(code, "C1") ||
		strings.HasPrefix(code, "D0") || strings.HasPrefix(code, "D1")
}

// ── step 3: partition ────────────────────────────────────────────────────────

func (g *Grouper) partitionProcs(procs []string) (bool, string) {
	for _, p := range procs {
		if isORProcedure(p) {
			return true, "SURG"
		}
	}
	return false, "MED"
}

// isORProcedure mirrors the Python CodeRegistry.is_or_procedure() logic.
func isORProcedure(code string) bool {
	if len(code) != 7 {
		return false
	}
	section := code[0]
	rootOp := code[2]
	approach := code[4]

	if section == '0' {
		majorOps := map[byte]bool{'1': true, '6': true, 'G': true, 'M': true, 'R': true, 'S': true, 'T': true, 'Y': true}
		if majorOps[rootOp] {
			return true
		}
		orOps := map[byte]bool{
			'2': true, '4': true, '5': true, '7': true, '8': true, '9': true,
			'B': true, 'C': true, 'D': true, 'F': true, 'H': true, 'J': true,
			'K': true, 'L': true, 'N': true, 'P': true, 'Q': true, 'U': true,
			'V': true, 'W': true, 'X': true,
		}
		switch approach {
		case '0', '4': // open or percutaneous endoscopic
			return orOps[rootOp]
		case '3': // percutaneous — cardiac cath counts as surgical
			bodySystem := code[1]
			if bodySystem == '2' {
				return true
			}
			return majorOps[rootOp]
		}
	}
	// section 5 = extracorporeal assistance
	if section == '5' {
		return true
	}
	// section X = new technology
	if section == 'X' {
		return true
	}
	return false
}

// ── step 4: complication resolution ─────────────────────────────────────────

func (g *Grouper) resolveComplications(pdx string, sdxs []string, _ string) (string, []string, []string) {
	var ccApplied, mccApplied []string

	for _, dx := range sdxs {
		// check exclusion
		if excluded, ok := g.exclusions[dx]; ok {
			if excluded[pdx] {
				continue
			}
		}
		if g.mccCodes[dx] {
			mccApplied = append(mccApplied, dx)
		} else if g.ccCodes[dx] {
			ccApplied = append(ccApplied, dx)
		}
	}

	if len(mccApplied) > 0 {
		return "MCC", ccApplied, mccApplied
	}
	if len(ccApplied) > 0 {
		return "CC", ccApplied, mccApplied
	}
	return "NONE", ccApplied, mccApplied
}

// ── step 5: DRG selection ────────────────────────────────────────────────────

func (g *Grouper) selectDRG(
	mdc, partition, comp string,
	procs []string,
	pdx string,
) (string, string, float64, *float64, *float64) {

	// surgical path — try procedure-specific family first
	if partition == "SURG" && len(procs) > 0 {
		if hit := g.drgFromProcedure(mdc, procs, comp); hit != nil {
			return hit[0], hit[1], strToFloat(hit[2]), strToFloatPtr(hit[3]), strToFloatPtr(hit[4])
		}
	}

	// medical path — try diagnosis-specific family
	if partition == "MED" && pdx != "" {
		if hit := g.drgFromDiagnosis(mdc, pdx, comp); hit != nil {
			return hit[0], hit[1], strToFloat(hit[2]), strToFloatPtr(hit[3]), strToFloatPtr(hit[4])
		}
	}

	// fallback — scan all DRGs in this MDC + partition, match by severity keyword
	candidates := g.store.DRGsByMDCAndType(mdc, partition)
	if len(candidates) == 0 {
		return "999", "NON RAGGRUPPABILE", 0, nil, nil
	}
	best := bestSeverityMatch(candidates, comp)
	if best == nil {
		return "999", "NON RAGGRUPPABILE", 0, nil, nil
	}
	return g.drgFields(best)
}

// drgFromDiagnosis maps principal dx to a DRG family.
func (g *Grouper) drgFromDiagnosis(mdc, pdx, comp string) []string {
	var family [3]string // mcc_drg, cc_drg, none_drg

	switch mdc {
	case "01":
		family = nervousFamily(pdx)
	case "04":
		family = respiratoryFamily(pdx)
	case "05":
		family = circulatoryFamily(pdx)
	case "06":
		family = giFamily(pdx)
	case "07":
		family = hepatobiliaryFamily(pdx)
	case "11":
		family = kidneyFamily(pdx)
	case "18":
		family = infectiousFamily(pdx)
	default:
		return nil
	}
	if family[0] == "" {
		return nil
	}
	return g.applyThreeWay(family, comp)
}

// drgFromProcedure maps procedure codes to a DRG family.
// Currently implements procedure-specific routing for MDC 05 (cardiac procedures);
// all other MDCs rely on the bestSeverityMatch fallback.
func (g *Grouper) drgFromProcedure(mdc string, procs []string, comp string) []string {
	for _, code := range procs {
		if len(code) != 7 {
			continue
		}
		if mdc == "05" {
			if fam := cardiacProcFamily(code); fam[0] != "" {
				return g.applyTwoWay(fam, comp)
			}
		}
	}
	return nil
}

func (g *Grouper) applyThreeWay(fam [3]string, comp string) []string {
	var code string
	switch comp {
	case "MCC":
		code = fam[0]
	case "CC":
		code = fam[1]
	default:
		code = fam[2]
	}
	return g.resolveDRG(code)
}

func (g *Grouper) applyTwoWay(fam [2]string, comp string) []string {
	code := fam[1]
	if comp == "MCC" {
		code = fam[0]
	}
	return g.resolveDRG(code)
}

func (g *Grouper) resolveDRG(code string) []string {
	ref, ok := g.store.LookupDRG(code)
	if ok {
		geo := floatToStr(ref.GeometricLOS)
		arith := floatToStr(ref.ArithmeticLOS)
		return []string{ref.Code, ref.Description, floatToStr(ref.Weight), geo, arith}
	}
	return []string{code, "", "0", "", ""}
}

func (g *Grouper) drgFields(e *DRGEntry) (string, string, float64, *float64, *float64) {
	var geo, arith *float64
	if e.GeometricLOS > 0 {
		v := e.GeometricLOS
		geo = &v
	}
	if e.ArithmeticLOS > 0 {
		v := e.ArithmeticLOS
		arith = &v
	}
	return e.Code, e.Description, e.Weight, geo, arith
}

// ── MDC families ─────────────────────────────────────────────────────────────
//
// Each family function returns [3]string{mcc_drg, cc_drg, none_drg}.
// For 2-tier DRGs (CMM / SENZA CMM) repeat the lower DRG for CC and NONE.
// For 2-tier DRGs (CC/CMM / SENZA CC/CMM) repeat the upper DRG for MCC and CC.
// For a single DRG (no CC split) repeat the same code in all three slots.

// nervousFamily covers MDC 01 – Malattie e disturbi del sistema nervoso (medici).
func nervousFamily(dx string) [3]string {
	switch {
	case hasAnyPrefix(dx, "I60", "I61", "I62", "I63"):
		// Emorragia intracranica / infarto cerebrale – 3 livelli CC
		return [3]string{"064", "065", "066"}
	case hasAnyPrefix(dx, "I65", "I66"):
		// Occlusione precrebrale senza infarto – 2 livelli (CMM / SENZA CMM)
		return [3]string{"067", "068", "068"}
	case strings.HasPrefix(dx, "G45"):
		// Ischemia transitoria – DRG unico
		return [3]string{"069", "069", "069"}
	case hasAnyPrefix(dx, "I67", "I68", "I69"):
		// Altri disturbi cerebrovascolari – 3 livelli CC
		return [3]string{"070", "071", "072"}
	case hasAnyPrefix(dx, "G50", "G51", "G52", "G53", "G54", "G55", "G56", "G57", "G58", "G59",
		"G60", "G61", "G62", "G63", "G64", "G65"):
		// Nervi cranici e periferici – 2 livelli (CMM / SENZA CMM)
		return [3]string{"073", "074", "074"}
	case hasAnyPrefix(dx, "A87", "B00", "B01", "B02"):
		// Meningite virale – 2 livelli (CON CC/CMM / SENZA CC/CMM)
		return [3]string{"075", "075", "076"}
	case hasAnyPrefix(dx, "R40", "R41"):
		// Stupore e coma non traumatico – 2 livelli (CMM / SENZA CMM)
		return [3]string{"080", "081", "081"}
	case hasAnyPrefix(dx, "G40", "G41", "R56"):
		// Convulsioni / epilessia – 2 livelli (CMM / SENZA CMM)
		return [3]string{"100", "101", "101"}
	case hasAnyPrefix(dx, "G43", "G44", "R51"):
		// Cefalee – 2 livelli (CMM / SENZA CMM)
		return [3]string{"102", "103", "103"}
	default:
		// Altri disturbi del sistema nervoso – 3 livelli CC
		return [3]string{"091", "092", "093"}
	}
}

// respiratoryFamily covers MDC 04 – Sistema respiratorio (medici).
func respiratoryFamily(dx string) [3]string {
	switch {
	case hasAnyPrefix(dx, "J40", "J41", "J42", "J43", "J44", "J47"):
		return [3]string{"190", "191", "192"}
	case hasAnyPrefix(dx, "J45", "J20", "J21"):
		// Bronchite e asma – 2 livelli (CON CC/CMM / SENZA CC/CMM)
		return [3]string{"202", "202", "203"}
	case hasAnyPrefix(dx, "J12", "J13", "J14", "J15", "J16", "J17", "J18"):
		return [3]string{"193", "194", "195"}
	case strings.HasPrefix(dx, "I26"):
		// Embolia polmonare – 2 livelli (CMM / SENZA CMM)
		return [3]string{"175", "176", "176"}
	case strings.HasPrefix(dx, "J96"):
		// Insufficienza respiratoria – DRG unico
		return [3]string{"189", "189", "189"}
	case hasAnyPrefix(dx, "J90", "J91"):
		return [3]string{"186", "187", "188"}
	case strings.HasPrefix(dx, "J93"):
		return [3]string{"199", "200", "201"}
	case strings.HasPrefix(dx, "J84"):
		return [3]string{"196", "197", "198"}
	case hasAnyPrefix(dx, "C33", "C34", "C38", "C39", "D02", "D14", "D38"):
		return [3]string{"180", "181", "182"}
	case hasAnyPrefix(dx, "S22", "S27"):
		return [3]string{"183", "184", "185"}
	}
	return [3]string{}
}

// circulatoryFamily covers MDC 05 – Sistema circolatorio (medici).
func circulatoryFamily(dx string) [3]string {
	switch {
	case hasAnyPrefix(dx, "I21", "I22"):
		return [3]string{"280", "281", "282"}
	case strings.HasPrefix(dx, "I50"):
		return [3]string{"291", "292", "293"}
	case hasAnyPrefix(dx, "I47", "I48", "I49"):
		return [3]string{"308", "309", "310"}
	case strings.HasPrefix(dx, "R07"):
		// Dolore toracico – DRG unico 313
		return [3]string{"313", "313", "313"}
	case strings.HasPrefix(dx, "R55"):
		// Sincope e collasso – DRG unico 312
		return [3]string{"312", "312", "312"}
	case hasAnyPrefix(dx, "I10", "I11", "I12", "I13", "I15", "I16"):
		// Ipertensione – 2 livelli (CMM / SENZA CMM)
		return [3]string{"304", "305", "305"}
	case strings.HasPrefix(dx, "I25"):
		// Aterosclerosi – 2 livelli (CMM / SENZA CMM)
		return [3]string{"302", "303", "303"}
	case strings.HasPrefix(dx, "I71"):
		return [3]string{"299", "300", "301"}
	}
	return [3]string{}
}

// giFamily covers MDC 06 – Sistema digerente (medici).
func giFamily(dx string) [3]string {
	switch {
	case hasAnyPrefix(dx, "K20", "K21", "K22", "K23"):
		// Disturbi esofagei maggiori – 3 livelli CC
		return [3]string{"368", "369", "370"}
	case hasAnyPrefix(dx, "C15", "C16", "C17", "C18", "C19", "C20", "C21",
		"C22", "C23", "C24", "C25", "C26"):
		// Neoplasia maligna digestiva – 3 livelli CC
		return [3]string{"374", "375", "376"}
	case hasAnyPrefix(dx, "K92"):
		// Emorragia GI – 3 livelli CC
		return [3]string{"377", "378", "379"}
	case hasAnyPrefix(dx, "K25", "K26", "K27", "K28"):
		// Ulcera peptica complicata – 3 livelli CC
		return [3]string{"380", "381", "382"}
	case hasAnyPrefix(dx, "K50", "K51"):
		// Malattia infiammatoria intestinale – 3 livelli CC
		return [3]string{"385", "386", "387"}
	case strings.HasPrefix(dx, "K56"):
		// Ostruzione GI – 3 livelli CC
		return [3]string{"388", "389", "390"}
	case hasAnyPrefix(dx, "K52", "K58", "K59"):
		// Esofagite, gastroenterite e altri – 2 livelli (CMM / SENZA CMM)
		return [3]string{"391", "392", "392"}
	default:
		// Altre diagnosi sistema digestivo – 3 livelli CC
		return [3]string{"393", "394", "395"}
	}
}

// hepatobiliaryFamily covers MDC 07 – Epatobiliare e pancreas (medici).
func hepatobiliaryFamily(dx string) [3]string {
	switch {
	case hasAnyPrefix(dx, "K70", "K74"):
		// Cirrosi ed epatite alcolica – 3 livelli CC
		return [3]string{"432", "433", "434"}
	case hasAnyPrefix(dx, "C22", "C23", "C24", "C25"):
		// Neoplasia maligna epatobiliare – 3 livelli CC
		return [3]string{"435", "436", "437"}
	case hasAnyPrefix(dx, "K85", "K86"):
		// Disturbi del pancreas – 3 livelli CC
		return [3]string{"438", "439", "440"}
	case hasAnyPrefix(dx, "K71", "K72", "K73", "K75", "K76"):
		// Disturbi del fegato – 3 livelli CC
		return [3]string{"441", "442", "443"}
	case hasAnyPrefix(dx, "K80", "K81", "K82", "K83"):
		// Vie biliari – 3 livelli CC
		return [3]string{"444", "445", "446"}
	}
	return [3]string{}
}

// kidneyFamily covers MDC 11 – Rene e tratto urinario (medici).
func kidneyFamily(dx string) [3]string {
	switch {
	case hasAnyPrefix(dx, "N17", "N18", "N19"):
		// Insufficienza renale – 3 livelli CC
		return [3]string{"682", "683", "684"}
	case hasAnyPrefix(dx, "C64", "C65", "C66", "C67", "C68"):
		// Neoplasie rene e tratto urinario – 3 livelli CC
		return [3]string{"686", "687", "688"}
	case hasAnyPrefix(dx, "N10", "N11", "N12", "N30"):
		// Infezioni rene e tratto urinario – 2 livelli (CMM / SENZA CMM)
		return [3]string{"689", "690", "690"}
	case hasAnyPrefix(dx, "N20", "N21", "N22"):
		// Calcoli urinari – 2 livelli (CMM / SENZA CMM)
		return [3]string{"693", "694", "694"}
	default:
		// Altre diagnosi rene e tratto urinario – 3 livelli CC
		return [3]string{"698", "699", "700"}
	}
}

// infectiousFamily covers MDC 18 – Malattie infettive e parassitarie (medici).
func infectiousFamily(dx string) [3]string {
	switch {
	case hasAnyPrefix(dx, "A40", "A41"):
		// Setticemia/sepsi grave senza VM – 2 livelli (CMM / SENZA CMM)
		return [3]string{"871", "872", "872"}
	case hasAnyPrefix(dx, "B00", "B01", "B02", "B03", "B04", "B05", "B06",
		"B07", "B08", "B09", "B10", "B19", "B25", "B26", "B27", "B34"):
		// Malattia virale – 2 livelli (CMM / SENZA CMM)
		return [3]string{"865", "866", "866"}
	default:
		// Altre malattie infettive – 3 livelli CC
		return [3]string{"867", "868", "869"}
	}
}

func cardiacProcFamily(code string) [2]string {
	sec := code[0]
	bsys := code[1]
	rop := code[2]
	approach := code[4]
	device := code[5]

	if sec == '0' && bsys == '2' && rop == '7' && approach == '3' {
		if device == 'D' || device == 'E' || device == 'T' {
			return [2]string{"321", "322"}
		}
		return [2]string{"250", "251"}
	}
	if sec == '0' && bsys == '2' && rop == '1' {
		return [2]string{"235", "236"}
	}
	if sec == '0' && bsys == '2' && rop == 'R' {
		bpart := code[3]
		if bpart == 'F' || bpart == 'G' || bpart == 'H' || bpart == 'J' {
			return [2]string{"216", "220"}
		}
	}
	return [2]string{}
}

// ── severity fallback ─────────────────────────────────────────────────────────

// bestSeverityMatch selects the most appropriate DRG from a list of candidates
// based on the complication level.  The Italian DRG descriptions use:
//   - "CON CMM"     = with MCC (Major Complication/Comorbidity)
//   - "CON CC"      = with CC  (Complication/Comorbidity)
//   - "CON CC/CMM"  = with CC or MCC (combined 2-tier tier)
//   - "SENZA CC/CMM" = without CC or MCC
//   - "SENZA CMM"   = without MCC  (used in some 2-tier families)
func bestSeverityMatch(candidates []DRGEntry, comp string) *DRGEntry {
	upper := make([]string, len(candidates))
	for i, c := range candidates {
		upper[i] = strings.ToUpper(c.Description)
	}

	switch comp {
	case "MCC":
		// First pass: dedicated MCC/CMM tier.
		for i, desc := range upper {
			if strings.Contains(desc, "CON CMM") || strings.Contains(desc, "WITH MCC") {
				return &candidates[i]
			}
		}
		// Second pass: combined CC/CMM tier (2-tier DRGs).
		for i, desc := range upper {
			if strings.Contains(desc, "CON CC/CMM") || strings.Contains(desc, "WITH CC/MCC") {
				return &candidates[i]
			}
		}

	case "CC":
		// First pass: dedicated CC tier (not combined with CMM).
		for i, desc := range upper {
			hasCombined := strings.Contains(desc, "CON CC/CMM") || strings.Contains(desc, "WITH CC/MCC")
			hasMCC := strings.Contains(desc, "CON CMM") || strings.Contains(desc, "WITH MCC")
			hasCC := strings.Contains(desc, "CON CC") || strings.Contains(desc, "WITH CC")
			if hasCC && !hasCombined && !hasMCC {
				return &candidates[i]
			}
		}
		// Second pass: combined CC/CMM tier (2-tier DRGs).
		for i, desc := range upper {
			if strings.Contains(desc, "CON CC/CMM") || strings.Contains(desc, "WITH CC/MCC") {
				return &candidates[i]
			}
		}
		// Third pass: without-MCC tier (CMM/SENZA CMM families: CC maps here).
		for i, desc := range upper {
			hasSenzaCCCMM := strings.Contains(desc, "SENZA CC/CMM") || strings.Contains(desc, "WITHOUT CC/MCC")
			if (strings.Contains(desc, "SENZA CMM") || strings.Contains(desc, "WITHOUT MCC")) && !hasSenzaCCCMM {
				return &candidates[i]
			}
		}

	default: // NONE
		// First pass: without CC/CMM tier.
		for i, desc := range upper {
			if strings.Contains(desc, "SENZA CC/CMM") || strings.Contains(desc, "WITHOUT CC/MCC") ||
				strings.Contains(desc, "W/O CC/MCC") {
				return &candidates[i]
			}
		}
		// Second pass: without CMM (2-tier CMM/SENZA CMM families).
		for i, desc := range upper {
			hasSenzaCCCMM := strings.Contains(desc, "SENZA CC/CMM") || strings.Contains(desc, "WITHOUT CC/MCC")
			if (strings.Contains(desc, "SENZA CMM") || strings.Contains(desc, "WITHOUT MCC")) && !hasSenzaCCCMM {
				return &candidates[i]
			}
		}
	}

	if len(candidates) > 0 {
		return &candidates[0]
	}
	return nil
}

// ── MDC classifier ────────────────────────────────────────────────────────────

// pdxMDCMap is the CMS ICD-10-CM code range → MDC mapping.
// Later entries override earlier ones for the same range (last match wins).
var pdxMDCMap = []struct{ lo, hi, mdc string }{
	// mdc 01 — nervous system
	{"A17", "A179", "01"},
	{"A321", "A321", "01"},
	{"A390", "A394", "01"},
	{"A50", "A509", "01"},
	{"A521", "A521", "01"},
	{"A80", "A89", "01"},
	{"B00", "B004", "01"},
	{"B01", "B011", "01"},
	{"B02", "B021", "01"},
	{"B26", "B262", "01"},
	{"B37", "B375", "01"},
	{"B38", "B384", "01"},
	{"B45", "B451", "01"},
	{"G00", "G99", "01"},
	{"F01", "F09", "01"},
	{"R40", "R4082", "01"},
	{"R41", "R419", "01"},
	{"R47", "R479", "01"},
	{"R55", "R55", "01"},
	{"R56", "R569", "01"},
	// mdc 02 — eye
	{"H00", "H59", "02"},
	{"B30", "B309", "02"},
	// mdc 03 — ear, nose, mouth, throat
	{"H60", "H95", "03"},
	{"J00", "J06", "03"},
	{"J30", "J39", "03"},
	{"K00", "K14", "03"},
	// mdc 04 — respiratory system
	{"J09", "J18", "04"},
	{"J20", "J22", "04"},
	{"J40", "J47", "04"},
	{"J60", "J70", "04"},
	{"J80", "J84", "04"},
	{"J85", "J86", "04"},
	{"J90", "J94", "04"},
	{"J95", "J95", "04"},
	{"J96", "J99", "04"},
	{"R04", "R049", "04"},
	{"R05", "R059", "04"},
	{"R06", "R069", "04"},
	{"R09", "R099", "04"},
	// mdc 05 — circulatory system
	{"I00", "I99", "05"},
	{"R00", "R03", "05"},
	{"R07", "R079", "05"},
	{"R57", "R579", "05"},
	{"R58", "R58", "05"},
	// mdc 06 — digestive system
	{"K20", "K95", "06"},
	{"R10", "R19", "06"},
	// mdc 07 — hepatobiliary system and pancreas
	{"K70", "K77", "07"},
	{"K80", "K87", "07"},
	{"B15", "B19", "07"},
	// mdc 08 — musculoskeletal system and connective tissue
	{"M00", "M99", "08"},
	{"S00", "S99", "08"},
	{"T20", "T32", "08"},
	// mdc 09 — skin, subcutaneous tissue, breast
	{"L00", "L99", "09"},
	{"N60", "N65", "09"},
	// mdc 10 — endocrine, nutritional, metabolic
	{"E00", "E89", "10"},
	{"R63", "R639", "10"},
	{"R73", "R739", "10"},
	// mdc 11 — kidney and urinary tract
	{"N00", "N39", "11"},
	{"R30", "R39", "11"},
	// mdc 12 — male reproductive system
	{"N40", "N53", "12"},
	// mdc 13 — female reproductive system
	{"N70", "N98", "13"},
	// mdc 14 — pregnancy, childbirth, puerperium
	{"O00", "O9A", "14"},
	// mdc 15 — newborns and neonates
	{"P00", "P96", "15"},
	// mdc 16 — blood, blood-forming organs, immunological
	{"D50", "D89", "16"},
	// mdc 17 — myeloproliferative diseases
	{"C81", "C96", "17"},
	{"D45", "D479", "17"},
	// mdc 18 — infectious and parasitic diseases
	{"A00", "B99", "18"},
	{"R50", "R509", "18"},
	{"R65", "R659", "18"},
	// mdc 19 — mental diseases and disorders
	{"F10", "F99", "19"},
	// mdc 20 — substance use disorders
	{"F10", "F19", "20"},
	{"T40", "T409", "20"},
	{"T51", "T519", "20"},
	// mdc 21 — injuries, poisonings, toxic effects
	{"S00", "T88", "21"},
	// mdc 22 — burns
	{"T20", "T32", "22"},
	// mdc 23 — factors influencing health status
	{"Z00", "Z99", "23"},
	// overrides (must be last — last match wins)
	{"B20", "B20", "25"},  // mdc 25 — HIV
	{"Z21", "Z21", "25"},
	{"Z33", "Z339", "14"}, // mdc 14 pregnancy Z-codes
	{"Z34", "Z349", "14"},
	{"Z3A", "Z3A49", "14"},
	{"Z38", "Z389", "15"}, // mdc 15 newborn Z-codes
}

// classifyMDC returns the MDC code and a short description for a principal dx.
func classifyMDC(pdx string) (string, string) {
	if pdx == "" {
		return "00", "Unassigned"
	}
	var hit string
	for _, r := range pdxMDCMap {
		if inRange(pdx, r.lo, r.hi) {
			hit = r.mdc
		}
	}
	if hit == "" {
		return "00", "Unassigned"
	}
	return hit, ""
}

// inRange checks whether code falls lexicographically in [lo, hi].
// Shorter codes are padded so sub-codes are included.
func inRange(code, lo, hi string) bool {
	code = strings.ToUpper(code)
	lo = strings.ToUpper(lo)
	hi = strings.ToUpper(hi)

	// truncate code to length of hi if longer
	prefix := code
	if len(code) > len(hi) {
		prefix = code[:len(hi)]
	}

	n := len(hi)
	padded := padRight(prefix, n, '0')
	loPadded := padRight(lo, n, '0')
	hiPadded := padRight(hi, n, '9')

	return loPadded <= padded && padded <= hiPadded
}

func padRight(s string, n int, ch byte) string {
	for len(s) < n {
		s += string(ch)
	}
	return s
}

// ── description helpers ───────────────────────────────────────────────────────

func (g *Grouper) diagDesc(code string) string {
	code = strings.TrimSpace(code)
	// exact lookup (works when code is already in ICD-10-IM format with dot)
	if e, ok := g.store.LookupICD10(code); ok {
		return e.Description
	}
	// form a dotted variant (e.g. "I2109" → "I21.09") then try progressively
	// shorter candidates so ICD-10-CM codes that are more specific than the
	// Italian ICD-10-IM still resolve (e.g. "I21.09" falls back to "I21.0").
	if len(code) > 3 {
		dotted := code[:3] + "." + code[3:]
		for i := len(dotted); i > 3; i-- {
			candidate := dotted[:i]
			if strings.HasSuffix(candidate, ".") {
				continue // never try a code ending with a dot
			}
			if e, ok := g.store.LookupICD10(candidate); ok {
				return e.Description
			}
		}
	}
	// 3-character category fallback (e.g. "I21")
	if len(code) >= 3 {
		if e, ok := g.store.LookupICD10(code[:3]); ok {
			return e.Description
		}
	}
	return ""
}

func (g *Grouper) pcsDescription(code string) string {
	if g.pcsDesc != nil {
		return g.pcsDesc[code]
	}
	return ""
}

// ── data loaders ─────────────────────────────────────────────────────────────

// loadSeverityList parses a CMS CC or MCC list file.
// Format: lines starting with a code+tab+description; header lines skipped.
func loadSeverityList(fsys fs.FS, path string) (map[string]bool, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	m := make(map[string]bool, 6000)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "TABLE") || strings.HasPrefix(line, "Diagnosis") {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) < 1 {
			continue
		}
		code := strings.TrimSpace(strings.ReplaceAll(parts[0], ".", ""))
		if len(code) >= 3 {
			m[strings.ToUpper(code)] = true
		}
	}
	return m, sc.Err()
}

var (
	// part1Re matches: leading-space, code, CC|MCC, collection:N codes
	part1Re = regexp.MustCompile(`(?m)^\s+([A-Z0-9]+)\s+(?:CC|MCC)\s+(\d{4}):\d`)
	collRe  = regexp.MustCompile(`^PDX collection (\d+)`)
)

// loadCCExclusions parses CMS table 6K, returning a map of
// cc_code → set of principal-dx codes that nullify it.
func loadCCExclusions(fsys fs.FS, path string) (map[string]map[string]bool, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	text := string(raw)

	// pass 1: cc_code -> collection id
	codeToCollection := make(map[string]string, 15000)
	for _, m := range part1Re.FindAllStringSubmatch(text, -1) {
		codeToCollection[strings.ToUpper(m[1])] = m[2]
	}

	// pass 2: collection id -> set of PDX codes
	collections := make(map[string]map[string]bool)
	var curColl string
	for _, line := range strings.Split(text, "\n") {
		if cm := collRe.FindStringSubmatch(strings.TrimSpace(line)); cm != nil {
			curColl = cm[1]
			collections[curColl] = make(map[string]bool)
			continue
		}
		if curColl != "" {
			stripped := strings.TrimSpace(line)
			if stripped == "" {
				curColl = ""
				continue
			}
			// first token is the pdx code
			fields := strings.Fields(stripped)
			if len(fields) > 0 {
				pdx := strings.ToUpper(fields[0])
				if len(pdx) >= 3 {
					collections[curColl][pdx] = true
				}
			}
		}
	}

	// pass 3: cc_code -> set of excluding PDX codes
	excl := make(map[string]map[string]bool, len(codeToCollection))
	for ccCode, collID := range codeToCollection {
		if pdxSet, ok := collections[collID]; ok {
			excl[ccCode] = pdxSet
		}
	}
	return excl, nil
}

// loadPCSDescriptions parses the ICD-10-PCS CSV (code,description, no header).
func loadPCSDescriptions(fsys fs.FS, path string) (map[string]string, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	m := make(map[string]string, 80000)
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if len(rec) >= 2 {
			code := strings.TrimSpace(strings.Trim(rec[0], `"`))
			desc := strings.TrimSpace(strings.Trim(rec[1], `"`))
			if code != "" {
				m[strings.ToUpper(code)] = desc
			}
		}
	}
	return m, nil
}

// ── utilities ─────────────────────────────────────────────────────────────────

func norm(code string) string {
	return strings.ToUpper(strings.TrimSpace(strings.ReplaceAll(code, ".", "")))
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func floatToStr(f float64) string {
	if f == 0 {
		return ""
	}
	return strings.TrimRight(strings.TrimRight(
		// simple float formatting without fmt to avoid import
		func() string {
			// use strconv-style formatting
			b := make([]byte, 0, 16)
			b = appendFloat(b, f)
			return string(b)
		}(), "0"), ".")
}

// appendFloat appends a float64 to a byte slice with up to 4 decimal places.
func appendFloat(b []byte, f float64) []byte {
	// simple manual float -> string
	neg := f < 0
	if neg {
		f = -f
		b = append(b, '-')
	}
	intPart := int64(f)
	fracPart := f - float64(intPart)

	// integer digits
	if intPart == 0 {
		b = append(b, '0')
	} else {
		digits := make([]byte, 0, 10)
		ip := intPart
		for ip > 0 {
			digits = append(digits, byte('0'+ip%10))
			ip /= 10
		}
		for i := len(digits) - 1; i >= 0; i-- {
			b = append(b, digits[i])
		}
	}
	if fracPart > 0 {
		b = append(b, '.')
		for i := 0; i < 4; i++ {
			fracPart *= 10
			d := int(fracPart)
			b = append(b, byte('0'+d))
			fracPart -= float64(d)
		}
	}
	return b
}

func strToFloat(s string) float64 {
	if s == "" {
		return 0
	}
	var f float64
	var neg bool
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	parts := strings.SplitN(s, ".", 2)
	for _, ch := range parts[0] {
		f = f*10 + float64(ch-'0')
	}
	if len(parts) == 2 {
		dec := float64(1)
		for _, ch := range parts[1] {
			dec /= 10
			f += float64(ch-'0') * dec
		}
	}
	if neg {
		f = -f
	}
	return f
}

func strToFloatPtr(s string) *float64 {
	if s == "" {
		return nil
	}
	v := strToFloat(s)
	if v == 0 {
		return nil
	}
	return &v
}
