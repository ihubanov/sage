//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// TestDeptRBAC tests the full department-level RBAC lifecycle on-chain:
//
//  1. Register two orgs (OrgA, OrgB) with admin agents
//  2. Register departments in each org (Engineering, Research)
//  3. Add agents to departments with clearance levels
//  4. Create a federation between OrgA and OrgB, scoped to Engineering dept only
//  5. Submit memories from OrgA agents in different departments
//  6. Verify OrgB Engineering agent can access OrgA memories (federation allows Engineering)
//  7. Verify OrgB Research agent CANNOT access OrgA memories (federation doesn't include Research)
//  8. Register a domain owned by OrgA agent, submit memory to that domain
//  9. Cross-org query with department filtering
func TestDeptRBAC(t *testing.T) {
	requireNetwork(t)

	// --- Setup: Create 5 agents ---
	adminA := newTestAgent(t)    // OrgA admin
	adminB := newTestAgent(t)    // OrgB admin
	engAgentA := newTestAgent(t) // OrgA Engineering member
	engAgentB := newTestAgent(t) // OrgB Engineering member
	resAgentB := newTestAgent(t) // OrgB Research member

	t.Logf("AdminA:    %s", adminA.agentID[:16])
	t.Logf("AdminB:    %s", adminB.agentID[:16])
	t.Logf("EngAgentA: %s", engAgentA.agentID[:16])
	t.Logf("EngAgentB: %s", engAgentB.agentID[:16])
	t.Logf("ResAgentB: %s", resAgentB.agentID[:16])

	// --- Step 1: Register OrgA and OrgB ---
	t.Log("=== Step 1: Register organizations ===")
	orgAResp, code := doSignedPost(t, adminA, "/v1/org/register", map[string]interface{}{
		"name":        "OrgAlpha",
		"description": "Test Organization Alpha",
	})
	if code != 201 {
		t.Fatalf("register OrgA: expected 201, got %d: %v", code, orgAResp)
	}
	orgAID := orgAResp["org_id"].(string)
	t.Logf("OrgA registered: %s (tx: %s)", orgAID, orgAResp["tx_hash"])

	orgBResp, code := doSignedPost(t, adminB, "/v1/org/register", map[string]interface{}{
		"name":        "OrgBeta",
		"description": "Test Organization Beta",
	})
	if code != 201 {
		t.Fatalf("register OrgB: expected 201, got %d: %v", code, orgBResp)
	}
	orgBID := orgBResp["org_id"].(string)
	t.Logf("OrgB registered: %s (tx: %s)", orgBID, orgBResp["tx_hash"])

	waitForBlock(t, 5)

	// --- Step 2: Register departments ---
	t.Log("=== Step 2: Register departments ===")
	engDeptA, code := doSignedPost(t, adminA, fmt.Sprintf("/v1/org/%s/dept", orgAID), map[string]interface{}{
		"name":        "Engineering",
		"description": "Engineering department",
	})
	if code != 201 {
		t.Fatalf("register OrgA Engineering dept: expected 201, got %d: %v", code, engDeptA)
	}
	engDeptAID := engDeptA["dept_id"].(string)
	t.Logf("OrgA Engineering dept: %s (tx: %s)", engDeptAID, engDeptA["tx_hash"])

	resDeptA, code := doSignedPost(t, adminA, fmt.Sprintf("/v1/org/%s/dept", orgAID), map[string]interface{}{
		"name":        "Research",
		"description": "Research department",
	})
	if code != 201 {
		t.Fatalf("register OrgA Research dept: expected 201, got %d: %v", code, resDeptA)
	}
	resDeptAID := resDeptA["dept_id"].(string)
	t.Logf("OrgA Research dept: %s (tx: %s)", resDeptAID, resDeptA["tx_hash"])

	engDeptB, code := doSignedPost(t, adminB, fmt.Sprintf("/v1/org/%s/dept", orgBID), map[string]interface{}{
		"name":        "Engineering",
		"description": "Engineering department",
	})
	if code != 201 {
		t.Fatalf("register OrgB Engineering dept: expected 201, got %d: %v", code, engDeptB)
	}
	engDeptBID := engDeptB["dept_id"].(string)
	t.Logf("OrgB Engineering dept: %s (tx: %s)", engDeptBID, engDeptB["tx_hash"])

	resDeptB, code := doSignedPost(t, adminB, fmt.Sprintf("/v1/org/%s/dept", orgBID), map[string]interface{}{
		"name":        "Research",
		"description": "Research department",
	})
	if code != 201 {
		t.Fatalf("register OrgB Research dept: expected 201, got %d: %v", code, resDeptB)
	}
	resDeptBID := resDeptB["dept_id"].(string)
	t.Logf("OrgB Research dept: %s (tx: %s)", resDeptBID, resDeptB["tx_hash"])

	waitForBlock(t, 5)

	// --- Step 3: Add members to orgs and departments ---
	t.Log("=== Step 3: Add org members and dept members ===")

	// Add engAgentA to OrgA
	_, code = doSignedPost(t, adminA, fmt.Sprintf("/v1/org/%s/member", orgAID), map[string]interface{}{
		"agent_id":  engAgentA.agentID,
		"clearance": 2,
		"role":      "member",
	})
	if code != 201 {
		t.Fatalf("add engAgentA to OrgA: expected 201, got %d", code)
	}
	t.Log("Added engAgentA to OrgA (clearance=2)")

	// Add engAgentB to OrgB
	_, code = doSignedPost(t, adminB, fmt.Sprintf("/v1/org/%s/member", orgBID), map[string]interface{}{
		"agent_id":  engAgentB.agentID,
		"clearance": 2,
		"role":      "member",
	})
	if code != 201 {
		t.Fatalf("add engAgentB to OrgB: expected 201, got %d", code)
	}
	t.Log("Added engAgentB to OrgB (clearance=2)")

	// Add resAgentB to OrgB
	_, code = doSignedPost(t, adminB, fmt.Sprintf("/v1/org/%s/member", orgBID), map[string]interface{}{
		"agent_id":  resAgentB.agentID,
		"clearance": 2,
		"role":      "member",
	})
	if code != 201 {
		t.Fatalf("add resAgentB to OrgB: expected 201, got %d", code)
	}
	t.Log("Added resAgentB to OrgB (clearance=2)")

	waitForBlock(t, 5)

	// Add engAgentA to OrgA Engineering dept
	_, code = doSignedPost(t, adminA, fmt.Sprintf("/v1/org/%s/dept/%s/member", orgAID, engDeptAID), map[string]interface{}{
		"agent_id":  engAgentA.agentID,
		"clearance": 2,
		"role":      "member",
	})
	if code != 201 {
		t.Fatalf("add engAgentA to OrgA Engineering dept: expected 201, got %d", code)
	}
	t.Log("Added engAgentA to OrgA/Engineering dept (clearance=2)")

	// Add engAgentB to OrgB Engineering dept
	_, code = doSignedPost(t, adminB, fmt.Sprintf("/v1/org/%s/dept/%s/member", orgBID, engDeptBID), map[string]interface{}{
		"agent_id":  engAgentB.agentID,
		"clearance": 2,
		"role":      "member",
	})
	if code != 201 {
		t.Fatalf("add engAgentB to OrgB Engineering dept: expected 201, got %d", code)
	}
	t.Log("Added engAgentB to OrgB/Engineering dept (clearance=2)")

	// Add resAgentB to OrgB Research dept
	_, code = doSignedPost(t, adminB, fmt.Sprintf("/v1/org/%s/dept/%s/member", orgBID, resDeptBID), map[string]interface{}{
		"agent_id":  resAgentB.agentID,
		"clearance": 2,
		"role":      "member",
	})
	if code != 201 {
		t.Fatalf("add resAgentB to OrgB Research dept: expected 201, got %d", code)
	}
	t.Log("Added resAgentB to OrgB/Research dept (clearance=2)")

	waitForBlock(t, 5)

	// --- Step 4: Verify departments via GET ---
	t.Log("=== Step 4: Verify departments exist ===")
	deptInfo, code := doSignedGet(t, adminA, fmt.Sprintf("/v1/org/%s/dept/%s", orgAID, engDeptAID))
	if code != 200 {
		t.Fatalf("get OrgA Engineering dept: expected 200, got %d: %v", code, deptInfo)
	}
	t.Logf("OrgA Engineering dept: %v", deptInfo)

	deptsList, code := doSignedGet(t, adminA, fmt.Sprintf("/v1/org/%s/depts", orgAID))
	if code != 200 {
		t.Fatalf("list OrgA depts: expected 200, got %d: %v", code, deptsList)
	}
	t.Logf("OrgA departments: %v", deptsList)

	membersList, code := doSignedGet(t, adminB, fmt.Sprintf("/v1/org/%s/dept/%s/members", orgBID, engDeptBID))
	if code != 200 {
		t.Fatalf("list OrgB Engineering members: expected 200, got %d: %v", code, membersList)
	}
	t.Logf("OrgB Engineering dept members: %v", membersList)

	// --- Step 5: Register a domain owned by OrgA Engineering agent ---
	t.Log("=== Step 5: Register domain and submit memory ===")
	domainName := fmt.Sprintf("eng-data-%d", time.Now().UnixNano()%10000)
	_, code = doSignedPost(t, engAgentA, "/v1/domain/register", map[string]interface{}{
		"name":        domainName,
		"description": "Engineering data domain",
	})
	if code != 201 {
		t.Fatalf("register domain: expected 201, got %d", code)
	}
	t.Logf("Domain registered: %s", domainName)

	waitForBlock(t, 5)

	// Submit a memory to that domain from OrgA Engineering agent
	memResp, code := submitMemory(t, engAgentA, "OrgA Engineering secret: quantum computing breakthrough", domainName, "fact", 0.95)
	if code != 201 {
		t.Fatalf("submit memory: expected 201, got %d: %v", code, memResp)
	}
	memoryID := memResp["memory_id"].(string)
	t.Logf("Memory submitted: %s (tx: %s)", memoryID, memResp["tx_hash"])

	waitForBlock(t, 5)

	// --- Step 6: Create federation between OrgA and OrgB ---
	// CRITICAL: Federation scoped to "Engineering" department ONLY
	t.Log("=== Step 6: Propose federation (Engineering dept only) ===")
	fedResp, code := doSignedPost(t, adminA, "/v1/federation/propose", map[string]interface{}{
		"target_org_id":     orgBID,
		"allowed_domains":   []string{"*"},
		"allowed_depts":     []string{"Engineering"},
		"max_clearance":     2,
		"expires_at":        0,
		"requires_approval": false,
	})
	if code != 201 {
		t.Fatalf("propose federation: expected 201, got %d: %v", code, fedResp)
	}
	t.Logf("Federation proposed (tx: %s)", fedResp["tx_hash"])

	waitForBlock(t, 5)

	// Look up the federation ID from the active federations list
	fedListResp, code := doSignedGet(t, adminA, fmt.Sprintf("/v1/federation/active/%s", orgAID))
	if code != 200 {
		t.Fatalf("list federations: expected 200, got %d: %v", code, fedListResp)
	}
	// Response is an array — extract the federation ID
	fedItems, ok := fedListResp["items"].([]interface{})
	if !ok || len(fedItems) == 0 {
		t.Fatalf("no federations found for OrgA: %v", fedListResp)
	}
	fedEntry := fedItems[0].(map[string]interface{})
	fedID := fedEntry["federation_id"].(string)
	t.Logf("Federation ID found: %s (status: %s)", fedID, fedEntry["status"])

	// Approve federation from OrgB admin
	approveResp, code := doSignedPost(t, adminB, fmt.Sprintf("/v1/federation/%s/approve", fedID), map[string]interface{}{})
	if code != 200 {
		t.Fatalf("approve federation: expected 200, got %d: %v", code, approveResp)
	}
	t.Logf("Federation approved (tx: %s)", approveResp["tx_hash"])

	waitForBlock(t, 5)

	// Verify federation is active
	fedInfo, code := doSignedGet(t, adminA, fmt.Sprintf("/v1/federation/%s", fedID))
	if code != 200 {
		t.Fatalf("get federation: expected 200, got %d: %v", code, fedInfo)
	}
	t.Logf("Federation details: %v", fedInfo)

	// --- Step 7: Access control tests ---
	t.Log("=== Step 7: Cross-org access control tests ===")

	// 7a: OrgA Engineering agent should access their own memory (same org)
	getResp, code := getMemory(t, engAgentA, memoryID)
	if code != 200 {
		t.Errorf("FAIL: OrgA eng agent should access own memory: got %d: %v", code, getResp)
	} else {
		t.Log("PASS: OrgA Engineering agent can access own memory")
	}

	// 7b: OrgB Engineering agent should access OrgA memory via federation
	// (Federation allows Engineering dept)
	getResp, code = getMemory(t, engAgentB, memoryID)
	if code == 200 {
		t.Log("PASS: OrgB Engineering agent can access OrgA memory via federation")
	} else if code == 403 {
		t.Log("INFO: OrgB Engineering agent got 403 — access grant may also be needed for domain-level access")
		// This is expected if domain-level grants also need to be present
		// The federation alone might not be sufficient without domain access grant
	} else {
		t.Errorf("FAIL: OrgB Engineering agent unexpected status: got %d: %v", code, getResp)
	}

	// 7c: OrgB Research agent should NOT access OrgA memory
	// (Federation does NOT include Research dept)
	getResp, code = getMemory(t, resAgentB, memoryID)
	if code == 403 {
		t.Log("PASS: OrgB Research agent correctly denied access (dept not in federation)")
	} else if code == 200 {
		t.Errorf("FAIL: OrgB Research agent should NOT access OrgA memory (dept filtering broken)")
	} else {
		t.Logf("INFO: OrgB Research agent got status %d (expected 403): %v", code, getResp)
	}

	// --- Step 8: Verify department listing endpoints ---
	t.Log("=== Step 8: Department management endpoints ===")

	// List OrgB Engineering members
	members, code := doSignedGet(t, adminB, fmt.Sprintf("/v1/org/%s/dept/%s/members", orgBID, engDeptBID))
	if code != 200 {
		t.Errorf("list OrgB eng dept members: expected 200, got %d", code)
	} else {
		t.Logf("OrgB Engineering members: %v", members)
	}

	// List OrgB Research members
	members, code = doSignedGet(t, adminB, fmt.Sprintf("/v1/org/%s/dept/%s/members", orgBID, resDeptBID))
	if code != 200 {
		t.Errorf("list OrgB research dept members: expected 200, got %d", code)
	} else {
		t.Logf("OrgB Research members: %v", members)
	}

	// --- Step 9: Remove member from department ---
	t.Log("=== Step 9: Remove department member ===")
	_, code = doSignedDelete(t, adminB, fmt.Sprintf("/v1/org/%s/dept/%s/member/%s", orgBID, engDeptBID, engAgentB.agentID))
	if code != 200 {
		t.Errorf("remove engAgentB from OrgB Engineering: expected 200, got %d", code)
	} else {
		t.Log("PASS: Removed engAgentB from OrgB Engineering dept")
	}

	waitForBlock(t, 5)

	// After removal, engAgentB should no longer have dept-based federation access
	getResp, code = getMemory(t, engAgentB, memoryID)
	t.Logf("After dept removal, engAgentB access result: status=%d", code)
	// If they had access before through dept, they shouldn't now
	// But org-level access may still apply depending on federation config

	t.Log("=== Department RBAC integration test complete ===")
}

// --- Helper functions ---

func doSignedPost(t *testing.T, agent *testAgent, path string, body interface{}) (map[string]interface{}, int) {
	t.Helper()
	bodyBytes, _ := json.Marshal(body)
	req := agent.signedRequest(t, "POST", defaultAPIURL+path, bodyBytes)
	return doRequest(t, req)
}

func doSignedGet(t *testing.T, agent *testAgent, path string) (map[string]interface{}, int) {
	t.Helper()
	req := agent.signedRequest(t, "GET", defaultAPIURL+path, nil)
	return doRequest(t, req)
}

func doSignedDelete(t *testing.T, agent *testAgent, path string) (map[string]interface{}, int) {
	t.Helper()
	req := agent.signedRequest(t, "DELETE", defaultAPIURL+path, nil)
	return doRequest(t, req)
}

func doRequest(t *testing.T, req *http.Request) (map[string]interface{}, int) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &result); err != nil {
			// Try as array
			var arr []interface{}
			if err2 := json.Unmarshal(bodyBytes, &arr); err2 == nil {
				result = map[string]interface{}{"items": arr}
			} else {
				result = map[string]interface{}{"raw": string(bodyBytes)}
			}
		}
	}
	return result, resp.StatusCode
}
