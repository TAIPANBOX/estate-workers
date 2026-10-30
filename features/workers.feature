Feature: Estate workers spend the monthly API credits, not the weekly plan limit
  # @decided 2026-10-30 Routine estate work runs on forge through `claude -p`
  # with a key of the Console workspace `workers`, so it draws the monthly API
  # credits of the Max plan and never the weekly limit of the subscription.
  # Haiku 5.5 by default, Sonnet 5.5 where judgment is needed, Opus 5.5 only
  # with a written reason, Fable never. A GitHub App opens pull requests; the
  # supervisor session merges them for the first month. Spend is capped at
  # $90 a month by the workspace limit.
  # @decided 2026-10-30 Before the first real run, one probe proves that the
  # promotional credits pay and the purchased balance does not move; until
  # that is confirmed the workers spend at most ten cents in total.

  Scenario: A run never reads a subscription login
    Given a task for an allowed template
    When the orchestrator builds the claude command line
    Then it starts with --bare and -p, has no bypass and carries no key
    # test: TestArgsAreBareAndPrintAndNeverCarryTheKey

  Scenario: The key reaches only the model process, once
    Given the parent process holds other credentials in its environment
    When the run's environment is built
    Then it is built from nothing, holds the key exactly once and scrubs it from subprocesses
    # test: TestEnvIsBuiltFromNothingAndCarriesTheKeyOnlyOnce, TestDoFeedsThePromptAndReadsTheResult

  Scenario: Nothing beyond ten cents is spent before billing is verified
    Given billing has not been confirmed in the Console
    When a task could cost more than what is left under ten cents
    Then it is deferred, stays in the queue and the model never starts
    # test: TestUnverifiedBillingCapsTotalSpendAtTenCents, TestUnverifiedBillingDefersARunAndKeepsTheTask, TestUnverifiedCapNeverExceedsTheCeilingEvenIfConfigSaysMore

  Scenario: The credit probe fits under the ceiling and costs enough to see
    Given billing has not been confirmed
    When the credit probe runs
    Then it has no tools, carries filler so its cost shows in cents, and a second probe is deferred
    # test: TestTheCreditProbeRunsUnderTheCeiling, TestCreditProbeStaysUnderTheUnverifiedCeiling

  Scenario: The month cap stops the workers and the day cap waits for tomorrow
    Given spend so far plus this run's cap would pass a cap
    When the budget decides
    Then a month cap refuses and stops, a day cap refuses without stopping
    # test: TestMonthCapRefusesAndStops, TestDayCapRefusesWithoutStopping, TestExactFitIsAllowed

  Scenario: Fable never runs and Opus needs a reason
    Given a task names a model
    When the model is checked
    Then Fable and Mythos are refused even if listed, and Opus needs a written reason
    # test: TestFableAndMythosAreRefusedWhateverTheSpelling, TestFableStaysRefusedEvenIfSomeoneAddsItToTheKnownList, TestOpusNeedsAReason

  Scenario: The model never holds a GitHub credential
    Given the push service alone holds the App key
    When it asks GitHub for a token
    Then the token is for one repository with contents and pull requests only
    # test: TestRepoTokenAsksForOneRepoAndTwoPermissions, TestPushOpensAPullRequestForAnAllowedChange

  Scenario: A change outside the allowed files is refused and stops the workers
    Given the model wrote a workflow file
    When the orchestrator and then the push service check the diff
    Then nothing is pushed and the workers stop
    # test: TestAForbiddenChangeIsRefusedAndStopsTheWorkers, TestPushRefusesAForbiddenPathAndPushesNothing, TestForbiddenPathsAreRefusedEvenIfAllowed

  Scenario: A secret is never pushed
    Given the secret scanner finds something in the branch
    When the push service handles the job
    Then nothing is pushed and the workers stop
    # test: TestPushRefusesWhenTheScannerFindsASecret, TestAPushServiceSecretFindingStops, TestPushRefusesWithoutAScannerOrWhenItFails

  Scenario: A forged base or head cannot smuggle a change past the checks
    Given a manifest names a base that is not on main, or a head the bundle does not carry
    When the push service handles the job
    Then it refuses before pushing
    # test: TestPushRefusesABaseThatIsNotOnTheRemote, TestPushRefusesAManifestHeadThatIsNotTheBundledBranch, TestPushRefusesManifestsThatDisagree

  Scenario: A stop file halts a running model and the spend is still recorded
    Given a model is running
    When a person writes the stop file
    Then the model gets SIGINT within seconds and the ledger keeps its cost
    # test: TestAStopFileCancelsARunningModelAndTheSpendIsStillRecorded, TestCancelSendsSIGINTAndKeepsTheCost, TestAStubbornRunIsKilledAfterTheGrace

  Scenario: The ledger may over-count, never under-count
    Given a run's cost is unknown, or a ledger line cannot be trusted
    When the spend is recorded or summed
    Then the unknown cost is charged at the run's cap and an untrusted ledger stops the workers
    # test: TestAnUnknownCostIsChargedAtTheRunCap, TestBilledCostFailsClosed, TestMalformedLedgerStops, TestMalformedLinesAreCountedNotSkippedSilently, TestAnUnreadableLedgerDefersAndStops

  Scenario: An interrupted run is charged and closed at the next start
    Given a crash left a task in running with a transcript
    When the orchestrator starts
    Then the run is charged from its transcript or its whole cap and closed as an error
    # test: TestAnInterruptedRunIsChargedAndClosedAtTheNextStart

  Scenario: Some repositories are never touched
    Given hermes-openwrt, the site and the workers' own repository are hard-denied
    When a config or a task names one
    Then it is refused, even under a template that allows every repository
    # test: TestHardDeniedRepoCannotBeListed, TestTasksThatReachTooFarAreRefused, TestAWildcardTemplateStillNeedsTheConfigToListTheRepo, TestATaskForADeniedRepoIsRefused

  Scenario: A hostile repository config cannot run commands
    Given a repository sets hooks and an fsmonitor
    When the orchestrator runs git on it
    Then none of them runs
    # test: TestHostileRepoConfigCannotRunCommands, TestOnlyHTTPSUnlessFileIsAllowed

  Scenario: Allowed tools cannot widen themselves
    Given a template lists tools
    When the template is loaded
    Then broad shells, network tools, git config injection and wildcard flags are refused
    # test: TestTemplateRefusesBroadOrDangerousTools, TestTemplateAcceptsPinnedReadOnlyGit

  Scenario: Gates run offline with nothing to steal
    Given the gates are code the model could have edited
    When they run
    Then they run without network, credentials or any writable path but the job's tree
    # test: TestArgsCutTheNetworkAndHideCredentials, TestRunStopsAtTheFirstFailingGate

  Scenario: A finished task leaves a pull request a person can check without the diff
    Given an allowed change and green gates
    When the task completes
    Then a pull request opens with a fixed report, a model co-author and a NOT proven section
    # test: TestOnceTakesADossierTaskToAPullRequest, TestBodyHasTheFixedShapeAndFencesTheModelReport

  Scenario: The service loads the very credential the installer stored
    Given a credential put on the host by the installer
    When the service starts
    Then it loads that file by its full path, and the uninstaller removes the same file
    # test: TestServicesLoadTheFilePutCredentialWrites

  Scenario: The model works in its own job tree and nowhere in the worker's state
    Given a task's repository sits under the worker's state directory
    When the model edits files there, directly or through a shell command
    Then edits in the job's repository, home and temp directory are allowed
    And the ledger, the queue, the runs, the caches, the git home and the context repositories stay denied
    # test: TestDenyGuardsTheWorkerStateButNotTheJobTree

  Scenario: A run gets exactly the built-in tools its template names
    Given minimal mode offers only a shell, read and edit by default
    When a template allows other tools
    Then the run names those built-in tools explicitly and no others
    # test: TestBuiltinToolsAreTheBaseNamesOfTheRules, TestArgsAreBareAndPrintAndNeverCarryTheKey

  Scenario: The shell sandbox works inside the drain service
    Given the drain service is hardened by systemd
    When the model runs a shell command
    Then bubblewrap can still open paths, because no filter in the service answers its calls with ENOSYS
    # test: TestDrainServiceLetsBubblewrapOpenPaths

  Scenario: The sandbox's own empty files never reach a pull request
    Given the shell sandbox leaves empty files such as .env and package.json in the working tree
    When the task's change is collected
    Then those empty new files are dropped and only the model's change is checked and pushed
    And a new file with content, whatever its name, is still checked and refused when it is not allowed
    # test: TestSandboxPlaceholdersDoNotReachTheDiff, TestOnlyEmptyNewFilesAreTreatedAsPlaceholders

  Scenario: The shell sandbox can mount what it needs inside the drain service
    Given the drain service hides container sockets and keeps no mounts over /proc
    When the model runs a shell command
    Then bubblewrap can mask the sockets and mount a fresh /proc
    # test: TestDrainServiceLetsBubblewrapOpenPaths

  Scenario: Only the drain service can reach the push service
    Given the push service listens on a unix socket
    When the drain service hands over a finished task
    Then it can reach the socket, and only members of its group may connect
    # test: TestPushSocketIsReachableByTheDrainServiceOnly
