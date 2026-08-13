package damessage

// Add returns the sum of two provider-neutral usage records. Token detail maps
// are merged by key and the receiver is never mutated.
func (usage Usage) Add(other Usage) Usage {
	result := usage
	result.InputTokens += other.InputTokens
	result.OutputTokens += other.OutputTokens
	result.TotalTokens += other.TotalTokens
	result.CostUSD += other.CostUSD
	result.InputDetails = addUsageDetails(usage.InputDetails, other.InputDetails)
	result.OutputDetails = addUsageDetails(usage.OutputDetails, other.OutputDetails)
	if result.Provider == "" {
		result.Provider = other.Provider
	} else if other.Provider != "" && result.Provider != other.Provider {
		result.Provider = ""
	}
	if result.Model == "" {
		result.Model = other.Model
	} else if other.Model != "" && result.Model != other.Model {
		result.Model = ""
	}
	if result.URL == "" {
		result.URL = other.URL
	} else if other.URL != "" && result.URL != other.URL {
		result.URL = ""
	}
	if result.StartedAt.IsZero() || (!other.StartedAt.IsZero() && other.StartedAt.Before(result.StartedAt)) {
		result.StartedAt = other.StartedAt
	}
	if other.FinishedAt.After(result.FinishedAt) {
		result.FinishedAt = other.FinishedAt
	}
	return result
}

// AggregateUsage sums model and nested purposed usage carried by messages.
func AggregateUsage(messages []Message) Usage {
	var result Usage
	for _, item := range messages {
		if item.Usage != nil {
			result = result.Add(*item.Usage)
		}
		for _, nested := range item.OtherUsage {
			result = result.Add(nested.Usage)
		}
	}
	return result
}

// LastUsage returns the last direct model usage record in message order.
func LastUsage(messages []Message) (Usage, bool) {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Usage != nil {
			return *messages[index].Usage, true
		}
	}
	return Usage{}, false
}

// ToolUseCounts counts normalized assistant tool calls by tool name.
func ToolUseCounts(messages []Message) map[string]int {
	counts := map[string]int{}
	for _, item := range messages {
		for _, call := range item.ToolCalls {
			counts[call.Name]++
		}
	}
	return counts
}

func addUsageDetails(left, right map[string]int) map[string]int {
	if len(left) == 0 && len(right) == 0 {
		return nil
	}
	result := make(map[string]int, len(left)+len(right))
	for key, value := range left {
		result[key] = value
	}
	for key, value := range right {
		result[key] += value
	}
	return result
}
