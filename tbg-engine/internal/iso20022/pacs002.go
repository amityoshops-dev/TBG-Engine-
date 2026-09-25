package iso20022

import "encoding/xml"

// Pacs002 models a minimal ISO 20022 pacs.002.001.10 (FIToFIPaymentStatusReport)
// inbound message used to receive settlement confirmation / UTR allocation or
// rejection from the clearing rail.
type Pacs002 struct {
	XMLName xml.Name     `xml:"Document"`
	Body    fiToFIStatus `xml:"FIToFIPmtStsRpt"`
}

type fiToFIStatus struct {
	GrpHdr      grpHdr       `xml:"GrpHdr"`
	TxInfAndSts txStatusInfo `xml:"TxInfAndSts"`
}

type txStatusInfo struct {
	OrgnlEndToEndID string `xml:"OrgnlEndToEndId"`
	TxSts           string `xml:"TxSts"` // e.g. ACSC (Settled), RJCT (Rejected), PDNG (Pending)
	UTR             string `xml:"AcctSvcrRef"`
}

// ParsePacs002 parses an inbound status report.
func ParsePacs002(data []byte) (*Pacs002, error) {
	var doc Pacs002
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// IsSettled reports whether the status code indicates successful settlement.
func (p *Pacs002) IsSettled() bool {
	return p.Body.TxInfAndSts.TxSts == "ACSC"
}

// IsRejected reports whether the status code indicates rejection by the
// clearing rail (triggers the AUTO_REVERSED state transition).
func (p *Pacs002) IsRejected() bool {
	return p.Body.TxInfAndSts.TxSts == "RJCT"
}

// IsPending reports whether the rail has not yet resolved the transaction
// (feeds the TIMEOUT_INDETERMINATE polling state).
func (p *Pacs002) IsPending() bool {
	return p.Body.TxInfAndSts.TxSts == "PDNG"
}
