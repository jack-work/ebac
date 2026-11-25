package forge

// The comment edit mutations, one per comment kind. The github-cache sync
// (comment_edit) and the one-shot `ebac comment-edit` send the same text.
const (
	EditIssueCommentMutation  = `mutation($c:ID!,$b:String!){updateIssueComment(input:{id:$c,body:$b}){issueComment{body}}}`
	EditReviewCommentMutation = `mutation($c:ID!,$b:String!){updatePullRequestReviewComment(input:{pullRequestReviewCommentId:$c,body:$b}){pullRequestReviewComment{body}}}`
)

// EditCommentMutation is the mutation for a review comment (review) or an
// issue/PR conversation comment; its variables are c (the node id) and b.
func EditCommentMutation(review bool) string {
	if review {
		return EditReviewCommentMutation
	}
	return EditIssueCommentMutation
}
