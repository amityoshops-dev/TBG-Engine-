package iso20022

import (
	"encoding/xml"
	"time"
)

// Pacs008 models a minimal ISO 20022 pacs.008.001.08 (FIToFICustomerCreditTransfer)
// message, sufficient for representing a single credit transfer leg dispatched
// to the RBI SFMS (RTGS/NEFT) or NPCI (IMPS) clearing switch.
type Pacs008 struct {
	XMLName xml.Name `xml:"Document"`
	Xmlns   string   `xml:"xmlns,attr"`
	Body    fiToFI   `xml:"FIToFICstmrCdtTrf"`
}

type fiToFI struct {
	GrpHdr grpHdr   `xml:"GrpHdr"`
	TxInf  cdtTrfTx `xml:"CdtTrfTxInf"`
}

type grpHdr struct {
	MsgID   string `xml:"MsgId"`
	CreDtTm string `xml:"CreDtTm"`
	NbOfTxs int    `xml:"NbOfTxs"`
}

type cdtTrfTx struct {
	PmtID          pmtID `xml:"PmtId"`
	IntrBkSttlmAmt amt   `xml:"IntrBkSttlmAmt"`
	Dbtr           party `xml:"Dbtr"`
	CdtrAgt        fiAgt `xml:"CdtrAgt"`
	Cdtr           party `xml:"Cdtr"`
	CdtrAcct       acct  `xml:"CdtrAcct"`
}

type pmtID struct {
	EndToEndID string `xml:"EndToEndId"`
}

type amt struct {
	Ccy   string  `xml:"Ccy,attr"`
	Value float64 `xml:",chardata"`
}

type party struct {
	Nm string `xml:"Nm"`
}

type fiAgt struct {
	FinInstnID finInstnID `xml:"FinInstnId"`
}

type finInstnID struct {
	BICFI string `xml:"BICFI"`
}

type acct struct {
	ID acctID `xml:"Id"`
}

type acctID struct {
	Othr othrID `xml:"Othr"`
}

type othrID struct {
	ID string `xml:"Id"`
}

// BuildPacs008 constructs a pacs.008.001.08 message for the given payout.
func BuildPacs008(msgID, endToEndID, debtorName, beneficiaryName, beneficiaryAcct, beneficiaryBIC, currency string, amount float64) ([]byte, error) {
	doc := Pacs008{
		Xmlns: "urn:iso:std:iso:20022:tech:xsd:pacs.008.001.08",
		Body: fiToFI{
			GrpHdr: grpHdr{
				MsgID:   msgID,
				CreDtTm: time.Now().UTC().Format(time.RFC3339),
				NbOfTxs: 1,
			},
			TxInf: cdtTrfTx{
				PmtID:          pmtID{EndToEndID: endToEndID},
				IntrBkSttlmAmt: amt{Ccy: currency, Value: amount},
				Dbtr:           party{Nm: debtorName},
				CdtrAgt:        fiAgt{FinInstnID: finInstnID{BICFI: beneficiaryBIC}},
				Cdtr:           party{Nm: beneficiaryName},
				CdtrAcct:       acct{ID: acctID{Othr: othrID{ID: beneficiaryAcct}}},
			},
		},
	}
	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), out...), nil
}
