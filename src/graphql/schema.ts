// GraphQL Schema and Resolvers

export const typeDefs = `
  scalar DateTime

  type Query {
    repository(owner: String!, name: String!): Repository
    repositoryOwner(login: String!): RepositoryOwner
    viewer: User
    rateLimit: RateLimit
  }

  type Mutation {
    createRepository(input: CreateRepositoryInput!): CreateRepositoryPayload
    createIssue(input: CreateIssueInput!): CreateIssuePayload
    createPullRequest(input: CreatePullRequestInput!): CreatePullRequestPayload
    mergePullRequest(input: MergePullRequestInput!): MergePullRequestPayload
    createLabel(input: CreateLabelInput!): CreateLabelPayload
  }

  type Repository {
    id: ID!
    databaseId: Int!
    name: String!
    nameWithOwner: String!
    owner: RepositoryOwner!
    description: String
    url: String!
    homepageUrl: String
    isPrivate: Boolean!
    isFork: Boolean!
    forkCount: Int!
    stargazerCount: Int!
    watchersCount: Int!
    issues(first: Int, after: String, states: [IssueState]): IssueConnection
    pullRequests(first: Int, after: String, states: [PullRequestState]): PullRequestConnection
    labels(first: Int, after: String): LabelConnection
    defaultBranchRef: BranchRef
    branches(first: Int, after: String): BranchConnection
    issues(first: Int, after: String, states: [IssueState]): IssueConnection
    pullRequests(first: Int, after: String, states: [PullRequestState]): PullRequestConnection
    issues(first: Int, after: String, states: [IssueState]): IssueConnection
  }

  type RepositoryOwner {
    id: ID!
    login: String!
    url: String!
    avatarUrl: String
  }

  type User {
    id: ID!
    login: String!
    name: String
    email: String
    avatarUrl: String
    url: String!
  }

  type RateLimit {
    limit: Int!
    cost: Int!
    remaining: Int!
    resetAt: DateTime!
  }

  type Issue {
    id: ID!
    number: Int!
    title: String!
    body: String
    state: IssueState!
    author: User
    assignees(first: Int): UserConnection
    labels(first: Int): LabelConnection
    createdAt: DateTime!
    updatedAt: DateTime!
    closedAt: DateTime
    repository: Repository!
  }

  type IssueConnection {
    edges: [IssueEdge!]!
    pageInfo: PageInfo!
    totalCount: Int!
  }

  type IssueEdge {
    node: Issue!
    cursor: String!
  }

  type PullRequest {
    id: ID!
    number: Int!
    title: String!
    body: String
    state: PullRequestState!
    headRefName: String!
    baseRefName: String!
    headRepository: Repository
    baseRepository: Repository!
    author: User
    mergeable: MergeableState
    merged: Boolean!
    mergedAt: DateTime
    mergedBy: User
    baseRef: BranchRef!
    headRef: BranchRef!
    createdAt: DateTime!
    updatedAt: DateTime!
    closedAt: DateTime
    mergedAt: DateTime
  }

  type PullRequestConnection {
    edges: [PullRequestEdge!]!
    pageInfo: PageInfo!
    totalCount: Int!
  }

  type PullRequestEdge {
    node: PullRequest!
    cursor: String!
  }

  type Label {
    id: ID!
    name: String!
    color: String!
    description: String
  }

  type LabelConnection {
    edges: [LabelEdge!]!
    pageInfo: PageInfo!
    totalCount: Int!
  }

  type LabelEdge {
    node: Label!
    cursor: String!
  }

  type BranchRef {
    name: String!
    target: Commit!
  }

  type BranchConnection {
    edges: [BranchEdge!]!
    pageInfo: PageInfo!
    totalCount: Int!
  }

  type BranchEdge {
    node: BranchRef!
    cursor: String!
  }

  type Commit {
    id: ID!
    oid: String!
    message: String!
    author: CommitAuthor!
    committedDate: DateTime!
  }

  type CommitAuthor {
    name: String!
    email: String!
    date: DateTime!
  }

  type PageInfo {
    hasNextPage: Boolean!
    hasPreviousPage: Boolean!
    startCursor: String
    endCursor: String
  }

  type UserConnection {
    edges: [UserEdge!]!
    pageInfo: PageInfo!
    totalCount: Int!
  }

  type UserEdge {
    node: User!
    cursor: String!
  }

  enum IssueState {
    OPEN
    CLOSED
  }

  enum PullRequestState {
    OPEN
    CLOSED
    MERGED
  }

  enum MergeableState {
    MERGEABLE
    CONFLICTING
    UNKNOWN
  }

  enum IssueState {
    OPEN
    CLOSED
  }

  enum PullRequestState {
    OPEN
    CLOSED
    MERGED
  }

  input CreateRepositoryInput {
    name: String!
    description: String
    private: Boolean
    hasIssuesEnabled: Boolean
    hasWikiEnabled: Boolean
  }

  type CreateRepositoryPayload {
    repository: Repository
    clientMutationId: String
  }

  input CreateIssueInput {
    repositoryId: ID!
    title: String!
    body: String
    labelIds: [ID!]
  }

  type CreateIssuePayload {
    issue: Issue
    clientMutationId: String
  }

  input CreatePullRequestInput {
    repositoryId: ID!
    baseRefName: String!
    headRefName: String!
    title: String!
    body: String
  }

  type CreatePullRequestPayload {
    pullRequest: PullRequest
    clientMutationId: String
  }

  input MergePullRequestInput {
    pullRequestId: ID!
    commitHeadline: String
    commitBody: String
    mergeMethod: MergeMethod
  }

  enum MergeMethod {
    MERGE
    SQUASH
    REBASE
  }

  type MergePullRequestPayload {
    pullRequest: PullRequest
    clientMutationId: String
  }

  input CreateLabelInput {
    repositoryId: ID!
    name: String!
    color: String!
    description: String
  }

  type CreateLabelPayload {
    label: Label
    clientMutationId: String
  }

  scalar DateTime
  scalar ID
`;

// GraphQL Resolvers (stub implementations)
export const resolvers = {
  Query: {
    repository: async (_: any, { owner, name }: { owner: string; name: string }) => {
      // Return repository by owner/name
      return null;
    },
    viewer: () => ({
      id: '1',
      login: 'user',
      name: 'User',
      email: 'user@example.com',
      avatarUrl: 'https://github.com/identicons/user.png',
      url: '/user'
    }),
    rateLimit: () => ({
      limit: 5000,
      cost: 1,
      remaining: 4999,
      resetAt: new Date(Date.now() + 3600000).toISOString()
    })
  },
  Mutation: {
    createRepository: async (_: any, { input }: { input: any }) => {
      return {
        repository: {
          id: '1',
          databaseId: 1,
          name: input.name,
          nameWithOwner: `owner/${input.name}`,
          description: input.description,
          url: `https://github.com/owner/${input.name}`,
          isPrivate: input.private || false,
          isFork: false,
          forkCount: 0,
          stargazerCount: 0,
          watchersCount: 0,
          owner: { login: 'owner', id: '1', url: '/owner', avatarUrl: '' },
          defaultBranchRef: { name: 'main', target: { oid: '0'.repeat(40) } }
        }
      };
    },
    createIssue: async (_: any, { input }: { input: any }) => ({
      issue: {
        id: '1',
        number: 1,
        title: input.title,
        body: input.body || '',
        state: 'OPEN',
        author: { login: 'user', id: '1' },
        repository: { name: 'repo', nameWithOwner: 'owner/repo' },
        createdAt: new Date().toISOString(),
        updatedAt: new Date().toISOString()
      }
    }),
    createPullRequest: async (_: any, { input }: { input: any }) => ({
      pullRequest: {
        id: '1',
        number: 1,
        title: input.title,
        body: input.body || '',
        state: 'OPEN',
        headRefName: input.headRefName,
        baseRefName: input.baseRefName,
        headRepository: { name: 'repo', nameWithOwner: 'owner/repo' },
        baseRepository: { name: 'repo', nameWithOwner: 'owner/repo' },
        author: { login: 'user', id: '1' },
        mergeable: 'MERGEABLE',
        merged: false,
        createdAt: new Date().toISOString(),
        updatedAt: new Date().toISOString()
      }
    }),
    mergePullRequest: async (_: any, { input }: { input: any }) => ({
      pullRequest: {
        id: input.pullRequestId,
        number: 1,
        title: 'PR',
        state: 'MERGED',
        merged: true,
        mergedAt: new Date().toISOString(),
        mergeable: 'MERGEABLE'
      }
    }),
    createLabel: async (_: any, { input }: { input: any }) => ({
      label: {
        id: '1',
        name: input.name,
        color: input.color,
        description: input.description
      }
    })
  }
};

export { typeDefs as schema };