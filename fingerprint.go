package asn1fp

import (
	"encoding/asn1"
	"errors"
	"github.com/prometheus/common/log"
	"go.uber.org/zap"
	"strconv"
	"strings"
)

var (
	oidExtensionCTPrecertificatePoison         = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 4, 3}
	oidExtensionAuthorityInfoAccess            = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 1}
	oidExtensionSubjectInfoAccess              = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 11}
	oidExtensionSignedCertificateTimestampList = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 4, 2}
)

type Config struct {
	IncludeExtensions bool
	ExcludeSubjNames  bool
	ExcludePrecert    bool
	ParseOID          bool
	Strict            bool
	Log               *zap.SugaredLogger
}

func Fingerprint(bytes []byte, c *Config) (string, error) {
	var fp string

	fps, err := fpRecurse(0, bytes, c)
	if err != nil {
		return fp, err
	}

	return strings.Join(fps, "\n") + "\n", nil
}

func fpForDepth(depth int, tag int) string {
	var str strings.Builder

	for i := 0; i < depth; i++ {
		str.WriteString("\t")
	}
	str.WriteString(strconv.Itoa(tag))

	return str.String()
}

func fpStrForDepth(depth int, tag string) string {
	var str strings.Builder

	for i := 0; i < depth; i++ {
		str.WriteString("\t")
	}
	str.WriteString(tag)

	return str.String()
}

const (
	SerialNumberTag = 2
	SignatureTag    = 16
	IssuerTag       = 16
	ValidityTag     = 16
	SubjectTag      = 16
	SpkiTag         = 16
)

//TODO: hacky way to check for subject / issuer name
func matchesTBSCertFormat(elements []*asn1.RawValue) bool {
	if len(elements) < 6 {
		return false
	}

	explicitVersion := elements[0].Tag == 0
	var indexOffset int
	if explicitVersion {
		indexOffset = 1
	} else {
		indexOffset = 0
	}

	if elements[0+indexOffset].Tag != SerialNumberTag {
		return false
	}

	if elements[1+indexOffset].Tag != SignatureTag {
		return false
	}

	if elements[2+indexOffset].Tag != IssuerTag {
		return false
	}
	if elements[3+indexOffset].Tag != ValidityTag {
		return false
	}

	if elements[4+indexOffset].Tag != SubjectTag {
		return false
	}

	if elements[5+indexOffset].Tag != SpkiTag {
		return false
	}

	return true
}

func parseOIDHandleBigInt(bytes []byte) (s asn1.ObjectIdentifier, err error) {
	oid, err := parseObjectIdentifier(bytes)

	if err == nil {
		return oid, nil
	}

	largeBase128Err := asn1.StructuralError{"base 128 integer too large"}
	if err == largeBase128Err {
		return asn1.ObjectIdentifier{6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6, 6}, nil
	}

	return oid, err

}

func fpRecurse(depth int, bytes []byte, c *Config) ([]string, error) {
	var obj asn1.RawValue

	rest, err := asn1.Unmarshal(bytes, &obj)
	if err != nil {
		return nil, err
	}

	if len(rest) > 0 && !c.IncludeExtensions {
		return nil, errors.New("fpRecurse: excess data")
	}

	fps := make([]string, 0)

	if obj.IsCompound {
		fps = append(fps, fpForDepth(depth, obj.Tag))

		//TODO: check for empty Sequence or Set
		elements, err := parseCompoundObj(obj.Bytes)
		if err != nil {
			c.Log.Fatal(err)
		}

		if depth == 1 && matchesTBSCertFormat(elements) {
			if c.ExcludeSubjNames {
				elements = append(elements[:5], elements[6:]...) // skip subject
				elements = append(elements[:3], elements[4:]...) // skip issuer
			} else {
				elements = append(elements[:3], elements[4:]...) // skip issuer
			}
		}

		// Detect extension object and handle each extension by OID
		if depth == 2 && obj.Tag == 3 && len(elements) == 1 && c.IncludeExtensions {
			currentDepth := depth + 1
			fps = append(fps, fpForDepth(currentDepth, elements[0].Tag))
			extensions, err := parseCompoundObj(elements[0].Bytes)
			if err != nil {
				c.Log.Fatal(err)
			}

			currentDepth += 1
			for _, obj := range extensions {
				fps = append(fps, fpForDepth(currentDepth, obj.Tag))
				extension, err := parseCompoundObj(obj.Bytes)
				if err != nil {
					c.Log.Fatal(err)
				}
				extOID, err := parseOIDHandleBigInt(extension[0].Bytes)
				if err != nil {
					c.Log.Fatal(err)
				}

				fps = append(fps, fpForDepth(currentDepth+1, extension[0].Tag)+"."+extOID.String())

				var extData *asn1.RawValue
				if len(extension) == 3 {
					fps = append(fps, fpForDepth(currentDepth+1, extension[1].Tag)) // extension critical bool val
					fps = append(fps, fpForDepth(currentDepth+1, extension[2].Tag))
					extData = extension[2]
				} else if len(extension) == 2 {
					fps = append(fps, fpForDepth(currentDepth+1, extension[1].Tag))
					extData = extension[1]
				} else {
					c.Log.Warn("Invalid extension length", len(extension))
				}

				if len(extOID) == 4 && extOID[0] == 2 && extOID[1] == 5 && extOID[2] == 29 {
					switch extOID[3] {
					// Extensions that can be handled generically
					// 35 - RFC 5280, 4.2.1.1. Authority Key Identifier
					// 	AuthorityKeyIdentifier ::= SEQUENCE {
					//		keyIdentifier             [0] KeyIdentifier           OPTIONAL,
					//		authorityCertIssuer       [1] GeneralNames            OPTIONAL,
					//		authorityCertSerialNumber [2] CertificateSerialNumber OPTIONAL  }
					// 14 - RFC 5280, 4.2.1.2. Subject Key Identifier
					//	SubjectKeyIdentifier ::= KeyIdentifier / OCTET STRING
					// 19 - RFC 5280, 4.2.1.9. Basic Constraints
					//	BasicConstraints ::= SEQUENCE {
					//	        cA                      BOOLEAN DEFAULT FALSE,
					//	        pathLenConstraint       INTEGER (0..MAX) OPTIONAL }
					// 37 - RFC 5280, 4.2.1.12. Extended Key Usage
					//  ExtKeyUsageSyntax ::= SEQUENCE SIZE (1..MAX) OF KeyPurposeId
					//
					//  KeyPurposeId ::= OBJECT IDENTIFIER
					case 35, 14, 19, 37:
						paths, err := fpRecurse(currentDepth+2, extData.Bytes, c)
						if err != nil {
							c.Log.Fatal(err)
						}
						fps = append(fps, paths...)

					// RFC 5280, 4.2.1.3. Key Usage
					//	KeyUsage ::= BIT STRING {
					//		digitalSignature        (0),
					//		nonRepudiation          (1), -- recent editions of X.509 have renamed this bit to contentCommitment
					//		keyEncipherment         (2),
					//		dataEncipherment        (3),
					//		keyAgreement            (4),
					//		keyCertSign             (5),
					//		cRLSign                 (6),
					//		encipherOnly            (7),
					//		decipherOnly            (8) }
					case 15:
						keyUsageExt, err := parseCompoundObj(extData.Bytes)
						if err != nil {
							c.Log.Fatal(err)
						}
						fps = append(fps, fpForDepth(currentDepth+2, keyUsageExt[0].Tag))
						bitString, err := parseBitString(keyUsageExt[0].Bytes)
						if err != nil {
							c.Log.Fatal(err)
						}
						fps = append(fps, fpStrForDepth(currentDepth+3, bitStringToString(bitString))) // custom bitstring print

					// RFC 5280, 4.2.1.4. Certificate Policies
					//	certificatePolicies ::= SEQUENCE SIZE (1..MAX) OF PolicyInformation
					//
					//   PolicyInformation ::= SEQUENCE {
					//        policyIdentifier   CertPolicyId,
					//        policyQualifiers   SEQUENCE SIZE (1..MAX) OF
					//                                PolicyQualifierInfo OPTIONAL }
					//
					//   CertPolicyId ::= OBJECT IDENTIFIER
					//
					//   PolicyQualifierInfo ::= SEQUENCE {
					//        policyQualifierId  PolicyQualifierId,
					//        qualifier          ANY DEFINED BY policyQualifierId }
					//
					//   -- policyQualifierIds for Internet policy qualifiers
					//
					//   id-qt          OBJECT IDENTIFIER ::=  { id-pkix 2 }
					//   id-qt-cps      OBJECT IDENTIFIER ::=  { id-qt 1 }
					//   id-qt-unotice  OBJECT IDENTIFIER ::=  { id-qt 2 }
					//
					//   PolicyQualifierId ::= OBJECT IDENTIFIER ( id-qt-cps | id-qt-unotice )
					//
					//   Qualifier ::= CHOICE {
					//        cPSuri           CPSuri,
					//        userNotice       UserNotice }
					//
					//   CPSuri ::= IA5String
					//
					//   UserNotice ::= SEQUENCE {
					//        noticeRef        NoticeReference OPTIONAL,
					//        explicitText     DisplayText OPTIONAL }
					//
					//   NoticeReference ::= SEQUENCE {
					//        organization     DisplayText,
					//        noticeNumbers    SEQUENCE OF INTEGER }
					//
					//   DisplayText ::= CHOICE {
					//        ia5String        IA5String      (SIZE (1..200)),
					//        visibleString    VisibleString  (SIZE (1..200)),
					//        bmpString        BMPString      (SIZE (1..200)),
					//        utf8String       UTF8String     (SIZE (1..200)) }
					case 32:
						cpExt, err := parseCompoundObj(extData.Bytes)
						if err != nil {
							c.Log.Fatal(err)
						}
						fps = append(fps, fpForDepth(currentDepth+2, cpExt[0].Tag))
						//TODO: customize CA-specific OID stuff

					// Extensions to not deep-dive for fingerprinting
					// RFC 5280, 4.2.1.5. Policy Mappings - ONLY for CA certificates
					// RFC 5280, 4.2.1.14. Inhibit anyPolicy
					case 33, 54:
						// do nothing

					// 17 - RFC 5280, 4.2.1.6. Subject Alternative Name
					//SubjectAltName ::= GeneralNames
					//
					//   GeneralNames ::= SEQUENCE SIZE (1..MAX) OF GeneralName
					//
					//   GeneralName ::= CHOICE {
					//        otherName                       [0]     OtherName,
					//        rfc822Name                      [1]     IA5String,
					//        dNSName                         [2]     IA5String,
					//        x400Address                     [3]     ORAddress,
					//        directoryName                   [4]     Name,
					//        ediPartyName                    [5]     EDIPartyName,
					//        uniformResourceIdentifier       [6]     IA5String,
					//        iPAddress                       [7]     OCTET STRING,
					//        registeredID                    [8]     OBJECT IDENTIFIER }
					//
					//   OtherName ::= SEQUENCE {
					//        type-id    OBJECT IDENTIFIER,
					//        value      [0] EXPLICIT ANY DEFINED BY type-id }
					//
					//   EDIPartyName ::= SEQUENCE {
					//        nameAssigner            [0]     DirectoryString OPTIONAL,
					//        partyName               [1]     DirectoryString }
					//
					// 18 - RFC 5280, 4.2.1.7. Issuer Alternative Name
					//   IssuerAltName ::= GeneralNames
					case 17, 18:
					//TODO: distinguish between the set of General Names used? Not number of names

					// RFC 5280, 4.2.1.8. Subject Directory Attributes
					// SubjectDirectoryAttributes ::= SEQUENCE SIZE (1..MAX) OF Attribute
					// Attribute               ::= SEQUENCE {
					//      type             AttributeType,
					//      values    SET OF AttributeValue }
					//            -- at least one value is required
					//
					// AttributeType           ::= OBJECT IDENTIFIER
					//
					// AttributeValue          ::= ANY -- DEFINED BY AttributeType
					//
					// AttributeTypeAndValue   ::= SEQUENCE {
					//         type    AttributeType,
					//         value   AttributeValue }
					case 9: //TODO: finish

					case 30: // RFC 5280, 4.2.1.10. Name Constraints
					case 36: // RFC 5280, 4.2.1.11. Policy Constraints

					// 31 - RFC 5280, 4.2.1.13. CRL Distribution Points
					// CRLDistributionPoints ::= SEQUENCE SIZE (1..MAX) OF DistributionPoint
					//
					//   DistributionPoint ::= SEQUENCE {
					//        distributionPoint       [0]     DistributionPointName OPTIONAL,
					//        reasons                 [1]     ReasonFlags OPTIONAL,
					//        cRLIssuer               [2]     GeneralNames OPTIONAL }
					//
					//   DistributionPointName ::= CHOICE {
					//        fullName                [0]     GeneralNames,
					//        nameRelativeToCRLIssuer [1]     RelativeDistinguishedName }
					// ReasonFlags ::= BIT STRING {
					//        unused                  (0),
					//        keyCompromise           (1),
					//        cACompromise            (2),
					//        affiliationChanged      (3),
					//        superseded              (4),
					//        cessationOfOperation    (5),
					//        certificateHold         (6),
					//        privilegeWithdrawn      (7),
					//        aACompromise            (8) }
					// 46 - RFC 5280, 4.2.1.15. Freshest CRL (a.k.a. Delta CRL Distribution Point)
					//    FreshestCRL ::= CRLDistributionPoints
					case 31, 46:
						crlDistExt, err := parseCompoundObj(extData.Bytes)
						if err != nil {
							c.Log.Fatal(err)
						}
						fps = append(fps, fpForDepth(currentDepth+2, crlDistExt[0].Tag))
						distPoints, err := parseCompoundObj(crlDistExt[0].Bytes)
						if err != nil {
							c.Log.Fatal(err)
						}
						for _, distPoint := range distPoints {
							fps = append(fps, fpForDepth(currentDepth+3, distPoint.Tag))
							elements, err := parseCompoundObj(distPoint.Bytes)
							if err != nil {
								c.Log.Fatal(err)
							}
							for _, element := range elements {
								fps = append(fps, fpForDepth(currentDepth+4, element.Tag))
								if element.Tag == 1 { // reasons ReasonFlag bit string
									bitString, err := parseBitString(element.Bytes)
									if err != nil {
										c.Log.Fatal(err)
									}
									fps = append(fps, fpStrForDepth(currentDepth+5, bitStringToString(bitString))) // custom bitstring print
								}
							}

						}

					default:
						c.Log.Warn("Extension with oid", extOID, "not found")
					}
				} else if extOID.Equal(oidExtensionAuthorityInfoAccess) || extOID.Equal(oidExtensionSubjectInfoAccess) {
					paths, err := fpRecurse(currentDepth+2, extData.Bytes, c)
					if err != nil {
						c.Log.Fatal(err)
					}
					fps = append(fps, paths...)
				} else if extOID.Equal(oidExtensionSignedCertificateTimestampList) {
					// TODO: parse SCT list
					paths, err := fpRecurse(currentDepth+2, extData.Bytes, c)
					if err != nil {
						c.Log.Fatal(err)
					}
					fps = append(fps, paths...)
				} else if extOID.Equal(oidExtensionCTPrecertificatePoison) {
					// do nothing!
				}
			}
		} else {
			for _, element := range elements {
				paths, err := fpRecurse(depth+1, element.FullBytes, c)
				if err != nil {
					switch err.(type) {
					case *excludePrecertErr:
						return nil, nil
					default:
						return nil, err
					}
				}

				fps = append(fps, paths...)
			}
		}

	} else {
		//	TODO: fix issue where GeneralName types [0-8] map to asn1 types in extensions
		switch obj.Tag {
		case asn1.TagBoolean,
			asn1.TagInteger,
			asn1.TagBitString,
			asn1.TagNull,
			asn1.TagEnum,
			asn1.TagUTF8String,
			asn1.TagNumericString,
			asn1.TagPrintableString,
			asn1.TagT61String,
			asn1.TagIA5String,
			asn1.TagUTCTime,
			asn1.TagGeneralizedTime,
			asn1.TagGeneralString,
			asn1.TagOctetString:
			fps = append(fps, fpForDepth(depth, obj.Tag))
		case asn1.TagOID:
			oid, err := parseOIDHandleBigInt(obj.Bytes)
			if err != nil {
				return nil, err
			}

			if c.ExcludePrecert && oid.Equal(oidExtensionCTPrecertificatePoison) {
				return nil, &excludePrecertErr{}
			}

			if c.ParseOID {
				if len(oid) > 15 {
					log.Debug("Skipping super long OID, likely a IA5 General Name")
					fps = append(fps, fpForDepth(depth, obj.Tag))
					return fps, nil
				}

				fps = append(fps, fpForDepth(depth, obj.Tag)+"."+oid.String())
			} else {
				fps = append(fps, fpForDepth(depth, obj.Tag))
			}

		default:
			if c.Strict {
				c.Log.Errorf("invalid simple ASN1 type: %d", obj.Tag)
				return nil, errors.New("invalid ASN1 type")
			}

			fps = append(fps, fpForDepth(depth, obj.Tag))
		}
	}

	return fps, nil
}

type excludePrecertErr struct{}

func (e *excludePrecertErr) Error() string {
	return "found precert"
}
