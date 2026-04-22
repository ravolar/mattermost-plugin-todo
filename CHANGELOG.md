# Changelog
All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](http://keepachangelog.com/en/1.0.0/)
and this project adheres to [Semantic Versioning](http://semver.org/spec/v2.0.0.html).

## Ravolar fork - 2026-04-22
### Fixed
- `change_assignment` now allows reassigning an accepted delegated todo from the current user's `My` list. This supports chains like `Marina -> Vadim -> Marina/Pete` instead of failing with `trying to change the assignment of a todo not owned`.

## Ravolar fork - 2026-04-21
### Fixed
- `change_assignment` bot DM now sends the receiver-side issue id, so the receiver can press `Accept` from the Mattermost message after a reassignment.

## Ravolar fork - 2026-04-20
### Fixed
- `Edit todo` modal state now follows fresh `issue.message` and `issue.description` from props while the item is not actively being edited.

## 0.0.1 - 2018-08-16
### Added
- Initial release
